// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/command/arguments"
	"github.com/opentofu/opentofu/internal/command/clistate"
	"github.com/opentofu/opentofu/internal/command/views"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/depsfile"
	"github.com/opentofu/opentofu/internal/encryption"
	"github.com/opentofu/opentofu/internal/initwd"
	"github.com/opentofu/opentofu/internal/plans"
	"github.com/opentofu/opentofu/internal/plans/planfile"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/terminal"
	"github.com/opentofu/opentofu/internal/tofu"
)

func TestLocal_planBasic(t *testing.T) {
	b := TestLocal(t)
	p := TestLocalProvider(t, b, "test", planFixtureSchema())

	op, done := testOperationPlan(t, "./testdata/plan")
	op.PlanRefresh = true

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}

	if !p.PlanResourceChangeCalled {
		t.Fatal("PlanResourceChange should be called")
	}

	// the backend should be unlocked after a run
	assertBackendStateUnlocked(t, b)

	if errOutput := done(t).Stderr(); errOutput != "" {
		t.Fatalf("unexpected error output:\n%s", errOutput)
	}
}

func TestLocal_planInAutomation(t *testing.T) {
	b := TestLocal(t)
	TestLocalProvider(t, b, "test", planFixtureSchema())

	const msg = `You didn't use the -out option`

	// When we're "in automation" we omit certain text from the plan output.
	// However, the responsibility for this omission is in the view, so here we
	// test for its presence while the "in automation" setting is false, to
	// validate that we are calling the correct view method.
	//
	// Ideally this test would be replaced by a call-logging mock view, but
	// that's future work.
	op, done := testOperationPlan(t, "./testdata/plan")
	op.PlanRefresh = true

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}

	if output := done(t).Stdout(); !strings.Contains(output, msg) {
		t.Fatalf("missing next-steps message when not in automation\nwant: %s\noutput:\n%s", msg, output)
	}
}

func TestLocal_planNoConfig(t *testing.T) {
	b := TestLocal(t)
	TestLocalProvider(t, b, "test", providers.ProviderSchema{})

	op, done := testOperationPlan(t, "./testdata/empty")
	op.PlanRefresh = true

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()

	output := done(t)

	if run.Result == backend.OperationSuccess {
		t.Fatal("plan operation succeeded; want failure")
	}

	if stderr := output.Stderr(); !strings.Contains(stderr, "No configuration files") {
		t.Fatalf("bad: %s", stderr)
	}

	// the backend should be unlocked after a run
	assertBackendStateUnlocked(t, b)
}

// This test validates the state lacking behavior when the inner call to
// Context() fails
func TestLocal_plan_context_error(t *testing.T) {
	b := TestLocal(t)

	// This is an intentionally-invalid value to make tofu.NewContext fail
	// when b.Operation calls it.
	// NOTE: This test was originally using a provider initialization failure
	// as its forced error condition, but tofu.NewContext is no longer
	// responsible for checking that. Invalid parallelism is the last situation
	// where tofu.NewContext can return error diagnostics, and arguably
	// we should be validating this argument at the UI layer anyway, so perhaps
	// in future we'll make tofu.NewContext never return errors and then
	// this test will become redundant, because its purpose is specifically
	// to test that we properly unlock the state if tofu.NewContext
	// returns an error.
	if b.ContextOpts == nil {
		b.ContextOpts = &tofu.ContextOpts{}
	}
	b.ContextOpts.Parallelism = -1

	op, done := testOperationPlan(t, "./testdata/plan")

	// we coerce a failure in Context() by omitting the provider schema
	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationFailure {
		t.Fatalf("plan operation succeeded")
	}

	// the backend should be unlocked after a run
	assertBackendStateUnlocked(t, b)

	if got, want := done(t).Stderr(), "Error: Invalid parallelism value"; !strings.Contains(got, want) {
		t.Fatalf("unexpected error output:\n%s\nwant: %s", got, want)
	}
}

func TestLocal_planOutputsChanged(t *testing.T) {
	b := TestLocal(t)
	testStateFile(t, b.StatePath, states.BuildState(func(ss *states.SyncState) {
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "changed"},
		}, cty.StringVal("before"), false, "")
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "sensitive_before"},
		}, cty.StringVal("before"), true, "")
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "sensitive_after"},
		}, cty.StringVal("before"), false, "")
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "removed"}, // not present in the config fixture
		}, cty.StringVal("before"), false, "")
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance,
			OutputValue: addrs.OutputValue{Name: "unchanged"},
		}, cty.StringVal("before"), false, "")
		// NOTE: This isn't currently testing the situation where the new
		// value of an output is unknown, because to do that requires there to
		// be at least one managed resource Create action in the plan and that
		// would defeat the point of this test, which is to ensure that a
		// plan containing only output changes is considered "non-empty".
		// For now we're not too worried about testing the "new value is
		// unknown" situation because that's already common for printing out
		// resource changes and we already have many tests for that.
	}))
	outDir := t.TempDir()
	defer os.RemoveAll(outDir)
	planPath := filepath.Join(outDir, "plan.tfplan")
	op, done := testOperationPlan(t, "./testdata/plan-outputs-changed")
	op.PlanRefresh = true
	op.PlanOutPath = planPath
	cfg := cty.ObjectVal(map[string]cty.Value{
		"path": cty.StringVal(b.StatePath),
	})
	cfgRaw, err := plans.NewDynamicValue(cfg, cfg.Type())
	if err != nil {
		t.Fatal(err)
	}
	op.PlanOutBackend = &plans.Backend{
		// Just a placeholder so that we can generate a valid plan file.
		Type:   "local",
		Config: cfgRaw,
	}
	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}
	if run.PlanEmpty {
		t.Error("plan should not be empty")
	}

	expectedOutput := strings.TrimSpace(`
Changes to Outputs:
  + added            = "after"
  ~ changed          = "before" -> "after"
  - removed          = "before" -> null
  ~ sensitive_after  = (sensitive value)
  ~ sensitive_before = (sensitive value)

You can apply this plan to save these new output values to the OpenTofu
state, without changing any real infrastructure.
`)

	if output := done(t).Stdout(); !strings.Contains(output, expectedOutput) {
		t.Errorf("Unexpected output:\n%s\n\nwant output containing:\n%s", output, expectedOutput)
	}
}

// Module outputs should not cause the plan to be rendered
func TestLocal_planModuleOutputsChanged(t *testing.T) {
	b := TestLocal(t)
	testStateFile(t, b.StatePath, states.BuildState(func(ss *states.SyncState) {
		ss.SetOutputValue(addrs.AbsOutputValue{
			Module:      addrs.RootModuleInstance.Child("mod", addrs.NoKey),
			OutputValue: addrs.OutputValue{Name: "changed"},
		}, cty.StringVal("before"), false, "")
	}))
	outDir := t.TempDir()
	defer os.RemoveAll(outDir)
	planPath := filepath.Join(outDir, "plan.tfplan")
	op, done := testOperationPlan(t, "./testdata/plan-module-outputs-changed")
	op.PlanRefresh = true
	op.PlanOutPath = planPath
	cfg := cty.ObjectVal(map[string]cty.Value{
		"path": cty.StringVal(b.StatePath),
	})
	cfgRaw, err := plans.NewDynamicValue(cfg, cfg.Type())
	if err != nil {
		t.Fatal(err)
	}
	op.PlanOutBackend = &plans.Backend{
		Type:   "local",
		Config: cfgRaw,
	}
	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}
	if !run.PlanEmpty {
		t.Fatal("plan should be empty")
	}

	expectedOutput := strings.TrimSpace(`
No changes. Your infrastructure matches the configuration.
`)
	if output := done(t).Stdout(); !strings.Contains(output, expectedOutput) {
		t.Fatalf("Unexpected output:\n%s\n\nwant output containing:\n%s", output, expectedOutput)
	}
}

func TestLocal_planTainted(t *testing.T) {
	b := TestLocal(t)
	p := TestLocalProvider(t, b, "test", planFixtureSchema())
	testStateFile(t, b.StatePath, testPlanState_tainted())
	outDir := t.TempDir()
	planPath := filepath.Join(outDir, "plan.tfplan")
	op, done := testOperationPlan(t, "./testdata/plan")
	op.PlanRefresh = true
	op.PlanOutPath = planPath
	cfg := cty.ObjectVal(map[string]cty.Value{
		"path": cty.StringVal(b.StatePath),
	})
	cfgRaw, err := plans.NewDynamicValue(cfg, cfg.Type())
	if err != nil {
		t.Fatal(err)
	}
	op.PlanOutBackend = &plans.Backend{
		// Just a placeholder so that we can generate a valid plan file.
		Type:   "local",
		Config: cfgRaw,
	}
	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}
	if !p.ReadResourceCalled {
		t.Fatal("ReadResource should be called")
	}
	if run.PlanEmpty {
		t.Fatal("plan should not be empty")
	}

	expectedOutput := `OpenTofu used the selected providers to generate the following execution
plan. Resource actions are indicated with the following symbols:
-/+ destroy and then create replacement

OpenTofu will perform the following actions:

  # test_instance.foo is tainted, so it must be replaced
-/+ resource "test_instance" "foo" {
        # (1 unchanged attribute hidden)

        # (1 unchanged block hidden)
    }

Plan: 1 to add, 0 to change, 1 to destroy.`
	if output := done(t).Stdout(); !strings.Contains(output, expectedOutput) {
		t.Fatalf("Unexpected output\ngot\n%s\n\nwant:\n%s", output, expectedOutput)
	}
}

func TestLocal_planDeposedOnly(t *testing.T) {
	b := TestLocal(t)
	p := TestLocalProvider(t, b, "test", planFixtureSchema())
	testStateFile(t, b.StatePath, states.BuildState(func(ss *states.SyncState) {
		ss.SetResourceInstanceDeposed(
			addrs.Resource{
				Mode: addrs.ManagedResourceMode,
				Type: "test_instance",
				Name: "foo",
			}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance),
			states.DeposedKey("00000000"),
			&states.ResourceInstanceObjectSrc{
				Status: states.ObjectReady,
				AttrsJSON: []byte(`{
				"ami": "bar",
				"network_interface": [{
					"device_index": 0,
					"description": "Main network interface"
				}]
			}`),
			},
			addrs.AbsProviderConfig{
				Provider: addrs.NewDefaultProvider("test"),
				Module:   addrs.RootModule,
			},
			addrs.NoKey,
		)
	}))
	outDir := t.TempDir()
	planPath := filepath.Join(outDir, "plan.tfplan")
	op, done := testOperationPlan(t, "./testdata/plan")
	op.PlanRefresh = true
	op.PlanOutPath = planPath
	cfg := cty.ObjectVal(map[string]cty.Value{
		"path": cty.StringVal(b.StatePath),
	})
	cfgRaw, err := plans.NewDynamicValue(cfg, cfg.Type())
	if err != nil {
		t.Fatal(err)
	}
	op.PlanOutBackend = &plans.Backend{
		// Just a placeholder so that we can generate a valid plan file.
		Type:   "local",
		Config: cfgRaw,
	}
	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}
	if !p.ReadResourceCalled {
		t.Fatal("ReadResource should've been called to refresh the deposed object")
	}
	if run.PlanEmpty {
		t.Fatal("plan should not be empty")
	}

	// The deposed object and the current object are distinct, so our
	// plan includes separate actions for each of them. This strange situation
	// is not common: it should arise only if OpenTofu fails during
	// a create-before-destroy when the "create" hasn't completed yet but
	// in a severe way that prevents the previous object from being restored
	// as "current".
	//
	// However, that situation was more common in some earlier OpenTofu
	// versions where deposed objects were not managed properly, so this
	// can arise when upgrading from an older version with deposed objects
	// already in the state.
	//
	// This is one of the few cases where we expose the idea of "deposed" in
	// the UI, including the user-unfriendly "deposed key" (00000000 in this
	// case) just so that users can correlate this with what they might
	// see in `tofu show` and in the subsequent apply output, because
	// it's also possible for there to be _multiple_ deposed objects, in the
	// unlikely event that create_before_destroy _keeps_ crashing across
	// subsequent runs.
	expectedOutput := `OpenTofu used the selected providers to generate the following execution
plan. Resource actions are indicated with the following symbols:
  + create
  - destroy

OpenTofu will perform the following actions:

  # test_instance.foo will be created
  + resource "test_instance" "foo" {
      + ami = "bar"

      + network_interface {
          + description  = "Main network interface"
          + device_index = 0
        }
    }

  # test_instance.foo (deposed object 00000000) will be destroyed
  # (left over from a partially-failed replacement of this instance)
  - resource "test_instance" "foo" {
      - ami = "bar" -> null

      - network_interface {
          - description  = "Main network interface" -> null
          - device_index = 0 -> null
        }
    }

Plan: 1 to add, 0 to change, 1 to destroy.`
	if output := done(t).Stdout(); !strings.Contains(output, expectedOutput) {
		t.Fatalf("Unexpected output\ngot\n%s\n\nwant:\n%s", output, expectedOutput)
	}
}

func TestLocal_planTainted_createBeforeDestroy(t *testing.T) {
	b := TestLocal(t)

	p := TestLocalProvider(t, b, "test", planFixtureSchema())
	testStateFile(t, b.StatePath, testPlanState_tainted())
	outDir := t.TempDir()
	planPath := filepath.Join(outDir, "plan.tfplan")
	op, done := testOperationPlan(t, "./testdata/plan-cbd")
	op.PlanRefresh = true
	op.PlanOutPath = planPath
	cfg := cty.ObjectVal(map[string]cty.Value{
		"path": cty.StringVal(b.StatePath),
	})
	cfgRaw, err := plans.NewDynamicValue(cfg, cfg.Type())
	if err != nil {
		t.Fatal(err)
	}
	op.PlanOutBackend = &plans.Backend{
		// Just a placeholder so that we can generate a valid plan file.
		Type:   "local",
		Config: cfgRaw,
	}
	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}
	if !p.ReadResourceCalled {
		t.Fatal("ReadResource should be called")
	}
	if run.PlanEmpty {
		t.Fatal("plan should not be empty")
	}

	expectedOutput := `OpenTofu used the selected providers to generate the following execution
plan. Resource actions are indicated with the following symbols:
+/- create replacement and then destroy

OpenTofu will perform the following actions:

  # test_instance.foo is tainted, so it must be replaced
+/- resource "test_instance" "foo" {
        # (1 unchanged attribute hidden)

        # (1 unchanged block hidden)
    }

Plan: 1 to add, 0 to change, 1 to destroy.`
	if output := done(t).Stdout(); !strings.Contains(output, expectedOutput) {
		t.Fatalf("Unexpected output\ngot\n%s\n\nwant:\n%s", output, expectedOutput)
	}
}

func TestLocal_planRefreshFalse(t *testing.T) {
	b := TestLocal(t)

	p := TestLocalProvider(t, b, "test", planFixtureSchema())
	testStateFile(t, b.StatePath, testPlanState())

	op, done := testOperationPlan(t, "./testdata/plan")

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}

	if p.ReadResourceCalled {
		t.Fatal("ReadResource should not be called")
	}

	if !run.PlanEmpty {
		t.Fatal("plan should be empty")
	}

	if errOutput := done(t).Stderr(); errOutput != "" {
		t.Fatalf("unexpected error output:\n%s", errOutput)
	}
}

func TestLocal_planDestroy(t *testing.T) {
	b := TestLocal(t)

	TestLocalProvider(t, b, "test", planFixtureSchema())
	testStateFile(t, b.StatePath, testPlanState())

	outDir := t.TempDir()
	planPath := filepath.Join(outDir, "plan.tfplan")

	op, done := testOperationPlan(t, "./testdata/plan")
	op.PlanMode = plans.DestroyMode
	op.PlanRefresh = true
	op.PlanOutPath = planPath
	cfg := cty.ObjectVal(map[string]cty.Value{
		"path": cty.StringVal(b.StatePath),
	})
	cfgRaw, err := plans.NewDynamicValue(cfg, cfg.Type())
	if err != nil {
		t.Fatal(err)
	}
	op.PlanOutBackend = &plans.Backend{
		// Just a placeholder so that we can generate a valid plan file.
		Type:   "local",
		Config: cfgRaw,
	}

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}

	if run.PlanEmpty {
		t.Fatal("plan should not be empty")
	}

	plan := testReadPlan(t, planPath)
	for _, r := range plan.Changes.Resources {
		if r.Action.String() != "Delete" {
			t.Fatalf("bad: %#v", r.Action.String())
		}
	}

	if errOutput := done(t).Stderr(); errOutput != "" {
		t.Fatalf("unexpected error output:\n%s", errOutput)
	}
}

func TestLocal_planDestroy_withDataSources(t *testing.T) {
	b := TestLocal(t)

	TestLocalProvider(t, b, "test", planFixtureSchema())
	testStateFile(t, b.StatePath, testPlanState_withDataSource())

	outDir := t.TempDir()
	planPath := filepath.Join(outDir, "plan.tfplan")

	op, done := testOperationPlan(t, "./testdata/destroy-with-ds")
	op.PlanMode = plans.DestroyMode
	op.PlanRefresh = true
	op.PlanOutPath = planPath
	cfg := cty.ObjectVal(map[string]cty.Value{
		"path": cty.StringVal(b.StatePath),
	})
	cfgRaw, err := plans.NewDynamicValue(cfg, cfg.Type())
	if err != nil {
		t.Fatal(err)
	}
	op.PlanOutBackend = &plans.Backend{
		// Just a placeholder so that we can generate a valid plan file.
		Type:   "local",
		Config: cfgRaw,
	}

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}

	if run.PlanEmpty {
		t.Fatal("plan should not be empty")
	}

	// Data source should still exist in the plan file
	plan := testReadPlan(t, planPath)
	if len(plan.Changes.Resources) != 2 {
		t.Fatalf("Expected exactly 1 resource for destruction, %d given: %q",
			len(plan.Changes.Resources), getAddrs(plan.Changes.Resources))
	}

	// Data source should not be rendered in the output
	expectedOutput := `OpenTofu will perform the following actions:

  # test_instance.foo[0] will be destroyed
  - resource "test_instance" "foo" {
      - ami = "bar" -> null

      - network_interface {
          - description  = "Main network interface" -> null
          - device_index = 0 -> null
        }
    }

Plan: 0 to add, 0 to change, 1 to destroy.`

	if output := done(t).Stdout(); !strings.Contains(output, expectedOutput) {
		t.Fatalf("Unexpected output:\n%s", output)
	}
}

func getAddrs(resources []*plans.ResourceInstanceChangeSrc) []string {
	addrs := make([]string, len(resources))
	for i, r := range resources {
		addrs[i] = r.Addr.String()
	}
	return addrs
}

func TestLocal_planOutPathNoChange(t *testing.T) {
	b := TestLocal(t)
	TestLocalProvider(t, b, "test", planFixtureSchema())
	testStateFile(t, b.StatePath, testPlanState())

	outDir := t.TempDir()
	planPath := filepath.Join(outDir, "plan.tfplan")

	op, done := testOperationPlan(t, "./testdata/plan")
	op.PlanOutPath = planPath
	cfg := cty.ObjectVal(map[string]cty.Value{
		"path": cty.StringVal(b.StatePath),
	})
	cfgRaw, err := plans.NewDynamicValue(cfg, cfg.Type())
	if err != nil {
		t.Fatal(err)
	}
	op.PlanOutBackend = &plans.Backend{
		// Just a placeholder so that we can generate a valid plan file.
		Type:   "local",
		Config: cfgRaw,
	}
	op.PlanRefresh = true

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}

	plan := testReadPlan(t, planPath)

	if !plan.Changes.Empty() {
		t.Fatalf("expected empty plan to be written")
	}

	if errOutput := done(t).Stderr(); errOutput != "" {
		t.Fatalf("unexpected error output:\n%s", errOutput)
	}
}

func testOperationPlan(t *testing.T, configDir string) (*backend.Operation, func(*testing.T) *terminal.TestOutput) {
	t.Helper()

	_, configLoader := initwd.MustLoadConfigForTests(t, configDir, "tests")

	streams, done := terminal.StreamsForTesting(t)
	view := views.NewOperation(arguments.ViewHuman, false, views.NewView(streams))

	// Many of our tests use an overridden "test" provider that's just in-memory
	// inside the test process, not a separate plugin on disk.
	depLocks := depsfile.NewLocks()
	depLocks.SetProviderOverridden(addrs.MustParseProviderSourceString("registry.opentofu.org/hashicorp/test"))

	return &backend.Operation{
		Type:            backend.OperationTypePlan,
		Encryption:      encryption.Disabled(),
		ConfigDir:       configDir,
		ConfigLoader:    configLoader,
		StateLocker:     clistate.NewNoopLocker(),
		View:            view,
		DependencyLocks: depLocks,
	}, done
}

// testPlanState is just a common state that we use for testing plan.
func testPlanState() *states.State {
	state := states.NewState()
	rootModule := state.RootModule()
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "foo",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"ami": "bar",
				"network_interface": [{
					"device_index": 0,
					"description": "Main network interface"
				}]
			}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		},
		addrs.NoKey,
	)
	return state
}

func testPlanState_withDataSource() *states.State {
	state := states.NewState()
	rootModule := state.RootModule()
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "foo",
		}.Instance(addrs.IntKey(0)),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"ami": "bar",
				"network_interface": [{
					"device_index": 0,
					"description": "Main network interface"
				}]
			}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		},
		addrs.NoKey,
	)
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.DataResourceMode,
			Type: "test_ds",
			Name: "bar",
		}.Instance(addrs.IntKey(0)),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectReady,
			AttrsJSON: []byte(`{
				"filter": "foo"
			}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		},
		addrs.NoKey,
	)
	return state
}

func testPlanState_tainted() *states.State {
	state := states.NewState()
	rootModule := state.RootModule()
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "foo",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status: states.ObjectTainted,
			AttrsJSON: []byte(`{
				"ami": "bar",
				"network_interface": [{
					"device_index": 0,
					"description": "Main network interface"
				}]
			}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		},
		addrs.NoKey,
	)
	return state
}

func testReadPlan(t *testing.T, path string) *plans.Plan {
	t.Helper()

	p, err := planfile.Open(path, encryption.PlanEncryptionDisabled())
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	plan, err := p.ReadPlan()
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	return plan
}

// planFixtureSchema returns a schema suitable for processing the
// configuration in testdata/plan . This schema should be
// assigned to a mock provider named "test".
func planFixtureSchema() providers.ProviderSchema {
	return providers.ProviderSchema{
		ResourceTypes: map[string]providers.Schema{
			"test_instance": {
				Block: &configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"ami": {Type: cty.String, Optional: true},
					},
					BlockTypes: map[string]*configschema.NestedBlock{
						"network_interface": {
							Nesting: configschema.NestingList,
							Block: configschema.Block{
								Attributes: map[string]*configschema.Attribute{
									"device_index": {Type: cty.Number, Optional: true},
									"description":  {Type: cty.String, Optional: true},
								},
							},
						},
					},
				},
			},
		},
		DataSources: map[string]providers.Schema{
			"test_ds": {
				Block: &configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"filter": {Type: cty.String, Required: true},
					},
				},
			},
		},
	}
}

func TestLocal_invalidOptions(t *testing.T) {
	b := TestLocal(t)
	TestLocalProvider(t, b, "test", planFixtureSchema())

	op, done := testOperationPlan(t, "./testdata/plan")
	op.PlanRefresh = true
	op.PlanMode = plans.RefreshOnlyMode
	op.ForceReplace = []addrs.AbsResourceInstance{mustResourceInstanceAddr("test_instance.foo")}

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	<-run.Done()
	if run.Result == backend.OperationSuccess {
		t.Fatalf("plan operation failed")
	}

	if errOutput := done(t).Stderr(); errOutput == "" {
		t.Fatal("expected error output")
	}
}
