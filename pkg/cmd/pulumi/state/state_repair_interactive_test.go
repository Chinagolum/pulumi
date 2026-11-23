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
	"runtime"
	"testing"

	pkgresource "github.com/pulumi/pulumi/pkg/v3/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testProviderType = "pulumi:providers:pkgA"
	testProviderURN  = "urn:pulumi:stack::project::pulumi:providers:pkgA::default"
	testResourceType = "pkgA:index:Resource"
	testResourceURN  = "urn:pulumi:stack::project::pkgA:index:Resource::res"
)

func testProvider(id string) *pkgresource.State {
	return &pkgresource.State{URN: testProviderURN, Type: testProviderType, Custom: true, ID: resource.ID(id)}
}

func testResource(id, providerID string) *pkgresource.State {
	return &pkgresource.State{
		URN:      testResourceURN,
		Type:     testResourceType,
		Custom:   true,
		ID:       resource.ID(id),
		Provider: testProviderURN + "::" + providerID,
	}
}

// scriptedPrompt returns a prompter that answers with the given choices in order, and records the messages it was
// asked.
func scriptedPrompt(t *testing.T, choices ...int) (stateRepairPrompter, *[]string) {
	var asked []string
	return func(msg string, options []string) (int, error) {
		asked = append(asked, msg)
		require.Less(t, len(asked)-1, len(choices), "unexpected prompt: %s", msg)
		return choices[len(asked)-1], nil
	}, &asked
}

func TestFindDuplicateResources(t *testing.T) {
	t.Parallel()

	a1 := &pkgresource.State{URN: "a", ID: "1"}
	a2 := &pkgresource.State{URN: "a", ID: "2"}
	aDeleted := &pkgresource.State{URN: "a", ID: "0", Delete: true}
	b := &pkgresource.State{URN: "b"}
	c1 := &pkgresource.State{URN: "c", ID: "1"}
	cDeleted := &pkgresource.State{URN: "c", ID: "0", Delete: true}

	duplicates := findDuplicateResources([]*pkgresource.State{aDeleted, a1, b, a2, c1, cDeleted})

	// Only "a" has more than one copy that isn't marked for deletion; "c" is a valid replacement in progress.
	require.Len(t, duplicates, 1)
	assert.Equal(t, resource.URN("a"), duplicates[0].URN)
	assert.Equal(t, []*pkgresource.State{a1, a2}, duplicates[0].Copies)
}

func TestFindDanglingProviderReferences(t *testing.T) {
	t.Parallel()

	provider := testProvider("real")
	res := testResource("r1", "missing")
	ok := &pkgresource.State{
		URN:      "urn:pulumi:stack::project::pkgA:index:Resource::ok",
		Type:     testResourceType,
		Custom:   true,
		ID:       "r2",
		Provider: testProviderURN + "::real",
	}

	dangling := findDanglingProviderReferences([]*pkgresource.State{provider, res, ok})

	require.Len(t, dangling, 1)
	assert.Equal(t, res, dangling[0].Resource)
	assert.Equal(t, []*pkgresource.State{provider}, dangling[0].Candidates)
}

//nolint:paralleltest // State repairing modifies the DisableIntegrityChecking global variable
func TestStateRepair_InteractivelyResolvesDuplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping: mocking input to Survey is not supported on Windows")
	}

	// Arrange.
	first := testResource("r1", "p1")
	second := testResource("r2", "p1")
	fx := newStateRepairCmdFixture(t, []*pkgresource.State{testProvider("p1"), first, second})

	prompt, asked := scriptedPrompt(t, 1) // keep the second copy
	fx.cmd.Prompt = prompt
	fx.stdin.buf.WriteString("yes\r\n")

	// Act.
	err := fx.cmd.run(t.Context())

	// Assert.
	require.NoError(t, err)
	require.Len(t, *asked, 1)
	assert.Contains(t, (*asked)[0], "appears 2 times")
	assert.Contains(t, fx.stdout.String(), "duplicate copies will be removed")
	assert.Contains(t, fx.stdout.String(), `id: r1`)

	require.NotNil(t, fx.imported, "Import should have proceeded")
	require.Len(t, fx.imported.Resources, 2)
	assert.Equal(t, resource.ID("r2"), fx.imported.Resources[1].ID)
}

//nolint:paralleltest // State repairing modifies the DisableIntegrityChecking global variable
func TestStateRepair_InteractivelyResolvesDanglingProvider(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping: mocking input to Survey is not supported on Windows")
	}

	// Arrange.
	fx := newStateRepairCmdFixture(t, []*pkgresource.State{testProvider("real"), testResource("r1", "missing")})

	prompt, asked := scriptedPrompt(t, 0) // use the only provider on offer
	fx.cmd.Prompt = prompt
	fx.stdin.buf.WriteString("yes\r\n")

	// Act.
	err := fx.cmd.run(t.Context())

	// Assert.
	require.NoError(t, err)
	require.Len(t, *asked, 1)
	assert.Contains(t, (*asked)[0], "which is not in the state")
	assert.Contains(t, fx.stdout.String(), "will be changed to use a different provider")

	require.NotNil(t, fx.imported, "Import should have proceeded")
	require.Len(t, fx.imported.Resources, 2)
	assert.Equal(t, testProviderURN+"::real", fx.imported.Resources[1].Provider)
}

//nolint:paralleltest // State repairing modifies the DisableIntegrityChecking global variable
func TestStateRepair_InteractiveRepairCanBeCancelled(t *testing.T) {
	// Arrange.
	fx := newStateRepairCmdFixture(t, []*pkgresource.State{
		testProvider("p1"), testResource("r1", "p1"), testResource("r2", "p1"),
	})

	prompt, _ := scriptedPrompt(t, 2) // "Cancel repair"
	fx.cmd.Prompt = prompt

	// Act.
	err := fx.cmd.run(t.Context())

	// Assert.
	require.NoError(t, err)
	assert.Contains(t, fx.stdout.String(), "cancelled")
	assert.Nil(t, fx.imported, "Import should not have proceeded")
}

//nolint:paralleltest // State repairing modifies the DisableIntegrityChecking global variable
func TestStateRepair_DoesNotPromptWithYes(t *testing.T) {
	// Arrange.
	fx := newStateRepairCmdFixture(t, []*pkgresource.State{
		testProvider("p1"), testResource("r1", "p1"), testResource("r2", "p1"),
	})
	fx.cmd.Args.Yes = true
	fx.cmd.Prompt = func(string, []string) (int, error) {
		t.Fatal("should not prompt when --yes is set")
		return 0, nil
	}

	// Act.
	err := fx.cmd.run(t.Context())

	// Assert.
	assert.ErrorContains(t, err, "duplicate resource")
	assert.Contains(t, fx.stderr.String(), "Failed to repair")
	assert.Nil(t, fx.imported, "Import should not have proceeded")
}

//nolint:paralleltest // State repairing modifies the DisableIntegrityChecking global variable
func TestStateRepair_FallsBackWhenNoProviderCanBeOffered(t *testing.T) {
	// Arrange. The missing provider has no provider of the same type to replace it with.
	fx := newStateRepairCmdFixture(t, []*pkgresource.State{testResource("r1", "missing")})
	fx.cmd.Prompt = func(string, []string) (int, error) {
		t.Fatal("should not prompt when there is nothing to choose from")
		return 0, nil
	}

	// Act.
	err := fx.cmd.run(t.Context())

	// Assert.
	assert.ErrorContains(t, err, "unknown provider")
	assert.Contains(t, fx.stderr.String(), "Failed to repair")
	assert.Nil(t, fx.imported, "Import should not have proceeded")
}
