// Copyright 2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package state

import (
	"errors"
	"fmt"
	"slices"
	"time"

	pkgresource "github.com/pulumi/pulumi/pkg/v3/resource"
	"github.com/pulumi/pulumi/pkg/v3/resource/deploy"
	"github.com/pulumi/pulumi/sdk/v3/go/common/providers"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

// errStateRepairCancelled is returned when the user chooses to cancel an interactive repair.
var errStateRepairCancelled = errors.New("state repair cancelled")

// stateRepairPrompter asks the user to pick one of the given options, returning the index of the chosen option.
type stateRepairPrompter func(msg string, options []string) (int, error)

// duplicateResource is a URN that appears more than once in a snapshot without all but one of its copies being marked
// for deletion. Repair can't tell which copy is correct, so the user has to choose.
type duplicateResource struct {
	URN resource.URN
	// The copies of the resource that are not marked for deletion, in snapshot order.
	Copies []*pkgresource.State
}

// danglingProviderReference is a resource whose provider reference points at a provider that is not in the snapshot.
// Every custom resource needs a provider, so repair can't just remove the reference; the user has to pick a provider.
type danglingProviderReference struct {
	Resource *pkgresource.State
	// The provider reference that couldn't be found.
	Reference providers.Reference
	// Providers in the snapshot of the same type as the missing one, which the resource could use instead.
	Candidates []*pkgresource.State
}

// providerReferenceChange records a provider reference that was rewritten during interactive repair.
type providerReferenceChange struct {
	URN resource.URN
	Old string
	New string
}

// removedDuplicate records a copy of a duplicate resource that was removed during interactive repair, along with the
// copy the user chose to keep.
type removedDuplicate struct {
	Removed *pkgresource.State
	Kept    *pkgresource.State
}

// interactiveRepairResult records the changes made during interactive repair, so that they can be summarised before
// the user confirms that the repaired state should be written.
type interactiveRepairResult struct {
	// Copies of duplicate resources that were removed from the snapshot.
	RemovedDuplicates []removedDuplicate
	// Provider references that were rewritten.
	ProviderChanges []providerReferenceChange
}

func (r *interactiveRepairResult) empty() bool {
	return r == nil || (len(r.RemovedDuplicates) == 0 && len(r.ProviderChanges) == 0)
}

// findDuplicateResources returns every URN with more than one copy that is not marked for deletion. This mirrors the
// duplicate check in Snapshot.VerifyIntegrity, which allows several copies of a URN only if all, or all but one, of
// them are marked for deletion.
func findDuplicateResources(resources []*pkgresource.State) []duplicateResource {
	live := map[resource.URN][]*pkgresource.State{}
	var order []resource.URN
	for _, state := range resources {
		if state.Delete {
			continue
		}
		if _, seen := live[state.URN]; !seen {
			order = append(order, state.URN)
		}
		live[state.URN] = append(live[state.URN], state)
	}

	var duplicates []duplicateResource
	for _, urn := range order {
		if copies := live[urn]; len(copies) > 1 {
			duplicates = append(duplicates, duplicateResource{URN: urn, Copies: copies})
		}
	}
	return duplicates
}

// findDanglingProviderReferences returns every resource whose provider reference points at a provider that is not in
// the snapshot, along with the providers of the same type that it could use instead. References that can't be parsed
// are skipped, since there's no way to tell which provider they were meant to refer to.
func findDanglingProviderReferences(resources []*pkgresource.State) []danglingProviderReference {
	known := map[providers.Reference]bool{}
	for _, state := range resources {
		if !providers.IsProviderType(state.Type) {
			continue
		}
		if ref, err := providers.NewReference(state.URN, state.ID); err == nil {
			known[ref] = true
		}
	}

	var dangling []danglingProviderReference
	for _, state := range resources {
		if state.Provider == "" || state.PendingReplacement {
			continue
		}
		ref, err := providers.ParseReference(state.Provider)
		if err != nil || known[ref] {
			continue
		}

		var candidates []*pkgresource.State
		for _, candidate := range resources {
			if candidate.Delete || candidate.Type != ref.URN().Type() {
				continue
			}
			if _, err := providers.NewReference(candidate.URN, candidate.ID); err != nil {
				continue
			}
			candidates = append(candidates, candidate)
		}

		dangling = append(dangling, danglingProviderReference{
			Resource:   state,
			Reference:  ref,
			Candidates: candidates,
		})
	}
	return dangling
}

// repairInteractively asks the user how to resolve the problems that automatic repair can't: duplicate resources and
// references to providers that are not in the snapshot. It modifies the snapshot in place and returns a record of the
// changes it made. If the user cancels, it returns errStateRepairCancelled.
func repairInteractively(snap *deploy.Snapshot, prompt stateRepairPrompter) (*interactiveRepairResult, error) {
	result := &interactiveRepairResult{}

	// Resolve duplicates first, since removing a duplicate provider can leave references to it dangling, which the
	// provider step below then picks up.
	for _, dup := range findDuplicateResources(snap.Resources) {
		options := make([]string, 0, len(dup.Copies)+1)
		for i, state := range dup.Copies {
			options = append(options, describeDuplicateCopy(i, state))
		}
		options = append(options, "Cancel repair")

		choice, err := prompt(fmt.Sprintf(
			"%s %q appears %d times in the state. Which copy should be kept?",
			dup.URN.Type(), dup.URN.Name(), len(dup.Copies)), options)
		if err != nil {
			return nil, err
		}
		if choice < 0 || choice >= len(dup.Copies) {
			return nil, errStateRepairCancelled
		}

		for i, state := range dup.Copies {
			if i == choice {
				continue
			}
			result.RemovedDuplicates = append(result.RemovedDuplicates, removedDuplicate{
				Removed: state,
				Kept:    dup.Copies[choice],
			})
			snap.Resources = slices.DeleteFunc(snap.Resources, func(s *pkgresource.State) bool { return s == state })
		}
	}

	for _, dangling := range findDanglingProviderReferences(snap.Resources) {
		// With no provider of the right type in the state there's nothing to offer, so leave this one for the
		// manual repair instructions.
		if len(dangling.Candidates) == 0 {
			continue
		}

		options := make([]string, 0, len(dangling.Candidates)+1)
		for _, candidate := range dangling.Candidates {
			options = append(options, fmt.Sprintf("%s (id: %s)", candidate.URN.Name(), candidate.ID))
		}
		options = append(options, "Cancel repair")

		choice, err := prompt(fmt.Sprintf(
			"%s %q uses the provider %q (id: %s), which is not in the state. Which provider should it use?",
			dangling.Resource.URN.Type(), dangling.Resource.URN.Name(),
			dangling.Reference.URN().Name(), dangling.Reference.ID()), options)
		if err != nil {
			return nil, err
		}
		if choice < 0 || choice >= len(dangling.Candidates) {
			return nil, errStateRepairCancelled
		}

		chosen := dangling.Candidates[choice]
		ref, err := providers.NewReference(chosen.URN, chosen.ID)
		if err != nil {
			return nil, fmt.Errorf("creating provider reference for %s: %w", chosen.URN, err)
		}

		result.ProviderChanges = append(result.ProviderChanges, providerReferenceChange{
			URN: dangling.Resource.URN,
			Old: dangling.Resource.Provider,
			New: ref.String(),
		})
		dangling.Resource.Provider = ref.String()
	}

	if result.empty() {
		return nil, nil
	}

	// A provider reference may now point at a provider that appears later in the snapshot, so sort again.
	if err := snap.Toposort(); err != nil {
		return nil, err
	}

	return result, nil
}

// describeDuplicateCopy returns a label for one copy of a duplicate resource that helps the user tell the copies
// apart.
func describeDuplicateCopy(index int, state *pkgresource.State) string {
	label := fmt.Sprintf("Copy %d: id %q", index+1, state.ID)
	if state.Created != nil {
		label += ", created " + state.Created.Format(time.RFC3339)
	}
	if state.Modified != nil {
		label += ", modified " + state.Modified.Format(time.RFC3339)
	}
	if state.Provider != "" {
		label += ", provider " + state.Provider
	}
	return label
}
