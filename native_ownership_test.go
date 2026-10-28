package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/stretchr/testify/require"
)

// This opt-in gate consumes the actual `agent build --native-only` output.
// It never builds a substitute agent, touches the user's registry, or selects
// processes by name. Both services and their agent owners are disposable.
func TestNativeAgentOwnershipLifecycle(t *testing.T) {
	agentHome := os.Getenv("FASTAPI_NATIVE_AGENT_HOME")
	cli := os.Getenv("FASTAPI_QUALIFICATION_CLI")
	if agentHome == "" && cli == "" {
		t.Skip("set FASTAPI_NATIVE_AGENT_HOME and FASTAPI_QUALIFICATION_CLI to qualify built artifacts")
	}
	if !filepath.IsAbs(agentHome) || !filepath.IsAbs(cli) {
		t.Fatal("qualification inputs must be absolute paths")
	}
	t.Setenv("HOME", t.TempDir()) // Core's process registry is OS-home scoped.
	t.Setenv(resources.CodeflyHomeEnv, agentHome)
	t.Setenv("GOWORK", "off")
	installed, err := agent.Path(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{installed, installed + ".cdx.json", cli} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("artifact %s sha256=%x", path, sha256.Sum256(body))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	a := startOwnedNativeService(t, ctx, "first")
	b := startOwnedNativeService(t, ctx, "sibling")
	assertNativeOwnership(t, ctx, a, true)
	assertNativeOwnership(t, ctx, b, true)

	// Ordinary Runtime.Stop must retire A without affecting B's exact birth.
	stopped, err := a.client.Stop(ctx, &runtimev0.StopRequest{})
	if err != nil || stopped.GetStatus().GetState() != runtimev0.StopStatus_SUCCESS {
		t.Fatalf("ordinary stop: %v, %v", stopped, err)
	}
	waitNativeCondition(t, func() bool { return !a.group.Alive() })
	if _, err := runners.LookupProcessGroup(a.group.PGID()); !errors.Is(err, runners.ErrProcessGroupNotRegistered) {
		t.Fatalf("ordinary stop retained the workload registration: %v", err)
	}
	assertNativeOwnership(t, ctx, b, true)
	t.Log("ordinary Stop removed first workload; sibling remains authenticated and supervised")

	// Re-Init after Stop released the environment, then restart through the
	// same Runtime and kill only its disposable owner
	// birth so no agent Stop/finalizer can mask recovery of an orphan workload.
	initialized, err := a.client.Init(ctx, a.init)
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, initialized.GetStatus().GetState(), initialized.GetStatus().GetMessage())
	startNativeWorkload(t, ctx, a)
	// A stale birth must be refused before SIGKILL delivery. If this guard
	// delivered to the live PID, the exact owner and workload checks would fail.
	stale := a.owner
	stale.StartID++
	require.ErrorContains(t, signalNativeOwner(ctx, stale), "owner identity changed")
	assertNativeOwnership(t, ctx, a, true)
	assertNativeOwnership(t, ctx, b, true)
	t.Log("stale owner refused with original agent and uvicorn births still alive")
	require.NoError(t, signalNativeOwner(ctx, a.owner))
	t.Logf("signaled owner only after full identity matched original: %+v", a.owner)
	waitNativeCondition(t, func() bool {
		ownership, err := a.group.InspectOwnership(ctx)
		return err == nil && ownership.Authenticated && !ownership.OwnerAlive
	})
	assertNativeOwnership(t, ctx, a, false)
	assertNativeOwnership(t, ctx, b, true)

	// Use the maintained CLI consumer and Core's strict receipt parser; do not
	// invent a recovery selection from registry JSON or a list of descendants.
	pgid := strconv.Itoa(a.group.PGID())
	preview, err := exec.CommandContext(ctx, cli, "recover", "process-group", pgid, "--preview").Output()
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	expected, err := runners.ParseProcessGroupRecoveryExpectation(preview)
	if err != nil || expected.Namespace != "authenticated-v2" || expected.PGID != a.group.PGID() || expected.Owner.PID != a.owner.PID || expected.Owner.BootID != a.owner.BootID || expected.Owner.StartID != a.owner.StartID {
		t.Fatalf("actual agent did not write the expected v2 registration: %+v, %v", expected, err)
	}
	t.Logf("orphan preview: %s", preview)
	assertNativeOwnership(t, ctx, a, false)
	receipt := filepath.Join(t.TempDir(), "recovery.json")
	if err := os.WriteFile(receipt, preview, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := exec.CommandContext(ctx, cli, "recover", "process-group", pgid, "--expect", receipt).CombinedOutput()
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, result)
	}
	if a.group.Alive() {
		t.Fatal("recovery left the selected workload alive")
	}
	if _, err := runners.LookupProcessGroup(a.group.PGID()); !errors.Is(err, runners.ErrProcessGroupNotRegistered) {
		t.Fatalf("recovery did not remove the exact registration: %v", err)
	}
	assertNativeOwnership(t, ctx, b, true)
	t.Logf("recovery result: %s; sibling preserved", result)
	stopped, err = b.client.Stop(ctx, &runtimev0.StopRequest{})
	if err != nil || stopped.GetStatus().GetState() != runtimev0.StopStatus_SUCCESS {
		t.Fatalf("sibling ordinary stop: %v, %v", stopped, err)
	}
	waitNativeCondition(t, func() bool { return !b.group.Alive() })
	_, err = runners.LookupProcessGroup(b.group.PGID())
	require.ErrorIs(t, err, runners.ErrProcessGroupNotRegistered)
}

type ownedNativeService struct {
	workspace string
	conn      *manager.AgentConn
	client    runtimev0.RuntimeClient
	group     *runners.TrackedProcessGroup
	workload  runners.ProcessIdentity
	init      *runtimev0.InitRequest
	owner     runners.ProcessIdentity
}

func startOwnedNativeService(t *testing.T, ctx context.Context, name string) *ownedNativeService {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeNativeFixture(t, filepath.Join(workspace, "service.codefly.yaml"), fmt.Sprintf(`name: %s
version: 0.0.1
agent:
  publisher: codefly.dev
  name: python-fastapi
  version: %s
spec:
  hot-reload: false
endpoints:
  - name: http
    api: http
    visibility: private
`, name, agent.Version))
	writeNativeFixture(t, filepath.Join(workspace, "code", "pyproject.toml"), uvicornProject)
	writeNativeFixture(t, filepath.Join(workspace, "code", "src", "main.py"), `import json, os
from pathlib import Path
Path("workload.json").write_text(json.dumps({"pid": os.getpid(), "pgid": os.getpgrp()}))
`+servingApp)
	t.Setenv(runners.LaunchWorkspaceEnv, workspace)
	conn, err := manager.Load(ctx, agent, manager.WithoutSandbox(), manager.WithoutPrincipal(), manager.WithWorkDir(workspace))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	// The manager itself starts a tracked group. Capture its authenticated
	// original leader while the managed connection is live, before any workload.
	ownerGroup, err := runners.LookupProcessGroup(conn.ProcessInfo().PID)
	require.NoError(t, err)
	owner := ownerGroup.Leader()
	require.Equal(t, conn.ProcessInfo().PID, owner.PID)
	ownership, err := ownerGroup.InspectOwnership(ctx)
	require.NoError(t, err)
	require.True(t, ownership.Authenticated)
	require.True(t, ownership.OwnerAlive)
	require.Equal(t, workspace, ownership.LaunchWorkspace)
	require.Contains(t, ownership.Members, owner)
	current, err := runners.InspectProcess(owner.PID)
	require.NoError(t, err)
	require.Equal(t, owner, current)
	t.Logf("captured authenticated agent owner: %+v", owner)
	client := runtimev0.NewRuntimeClient(conn.GRPCConn())
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = client.Stop(cleanup, &runtimev0.StopRequest{})
	})
	environment, err := resources.LocalEnvironment().Proto()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := client.Load(ctx, &runtimev0.LoadRequest{
		Identity:    &basev0.ServiceIdentity{Name: name, Module: "qualification", Version: "0.0.1", WorkspacePath: workspace, RelativeToWorkspace: "."},
		Environment: environment, DisableCatch: true,
	})
	if err != nil || loaded.GetStatus().GetState() != runtimev0.LoadStatus_READY {
		t.Fatalf("load: %v, %v\n%s", loaded, err, conn.StderrTail())
	}
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()
	identity := resources.ServiceIdentityFromProto(&basev0.ServiceIdentity{Name: name, Module: "qualification", Version: "0.0.1", WorkspacePath: workspace, RelativeToWorkspace: "."})
	mappings, err := networkManager.GenerateNetworkMappings(ctx, resources.LocalEnvironment(), &resources.Workspace{Name: "qualification"}, identity, loaded.GetEndpoints(), resources.NewRuntimeContextNative())
	require.NoError(t, err)
	initRequest := &runtimev0.InitRequest{RuntimeContext: resources.NewRuntimeContextNative(), ProposedNetworkMappings: mappings}
	initialized, err := client.Init(ctx, initRequest)
	if err != nil || initialized.GetStatus().GetState() != runtimev0.InitStatus_READY {
		t.Fatalf("init: %v, %v\n%s", initialized, err, conn.StderrTail())
	}
	service := &ownedNativeService{workspace: workspace, conn: conn, client: client, init: initRequest, owner: owner}
	startNativeWorkload(t, ctx, service)
	return service
}

func startNativeWorkload(t *testing.T, ctx context.Context, service *ownedNativeService) {
	t.Helper()
	pidFile := filepath.Join(service.workspace, "code", "workload.json")
	if err := os.Remove(pidFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	started, err := service.client.Start(ctx, &runtimev0.StartRequest{})
	if err != nil || started.GetStatus().GetState() != runtimev0.StartStatus_STARTED {
		t.Fatalf("start: %v, %v", started, err)
	}
	executable := started.GetStatus().GetStartedArtifact().GetExecutable()
	if !filepath.IsAbs(executable) {
		t.Fatalf("start did not report an absolute executable: %q", executable)
	}
	var witness struct {
		PID  int `json:"pid"`
		PGID int `json:"pgid"`
	}
	waitNativeCondition(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		err = json.Unmarshal(data, &witness)
		return err == nil && witness.PID > 1 && witness.PGID > 1
	})
	group, err := runners.LookupProcessGroup(witness.PGID)
	if err != nil {
		t.Fatal(err)
	}
	service.group = group
	service.workload, err = runners.InspectProcess(witness.PID)
	require.NoError(t, err)
	assertNativeOwnership(t, ctx, service, true)
	requireDeliverable(t, started.GetStatus().GetStartedArtifact())
	t.Logf("serving uvicorn birth: %+v; group leader: %+v; reported interpreter: %s", service.workload, group.Leader(), executable)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = group.Terminate(cleanup)
		_ = group.RemoveIfDeadContext(cleanup)
	})
}

// signalNativeOwner is only the fixture's admission guard. Core owns delivery
// and rechecks the current full identity at the kernel-bound signal seam.
func signalNativeOwner(ctx context.Context, original runners.ProcessIdentity) error {
	current, err := runners.InspectProcess(original.PID)
	if err != nil {
		return err
	}
	if current != original {
		return fmt.Errorf("owner identity changed: original=%+v current=%+v", original, current)
	}
	return runners.SignalProcess(ctx, current, syscall.SIGKILL)
}

func assertNativeOwnership(t *testing.T, ctx context.Context, service *ownedNativeService, ownerAlive bool) {
	t.Helper()
	ownership, err := service.group.InspectOwnership(ctx)
	require.NoError(t, err)
	require.True(t, ownership.Authenticated)
	require.Equal(t, ownerAlive, ownership.OwnerAlive)
	require.Equal(t, service.workspace, ownership.LaunchWorkspace)
	require.True(t, slices.Contains(ownership.Members, service.workload), "exact uvicorn birth absent: %+v", ownership)
	birth, err := runners.InspectProcess(service.workload.PID)
	require.NoError(t, err)
	require.Equal(t, service.workload, birth)
	if ownerAlive {
		current, err := runners.InspectProcess(service.owner.PID)
		require.NoError(t, err)
		require.Equal(t, service.owner, current, "managed owner birth changed")
	}
}

func writeNativeFixture(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
}

func waitNativeCondition(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("native lifecycle postcondition did not arrive")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
