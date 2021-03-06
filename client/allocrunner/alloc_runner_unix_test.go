// +build !windows

package allocrunner

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/nomad/client/consul"
	"github.com/hashicorp/nomad/client/state"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/stretchr/testify/require"
)

// TestAllocRunner_Restore_RunningTerminal asserts that restoring a terminal
// alloc with a running task properly kills the running the task. This is meant
// to simulate a Nomad agent crash after receiving an updated alloc with
// DesiredStatus=Stop, persisting the update, but crashing before terminating
// the task.
func TestAllocRunner_Restore_RunningTerminal(t *testing.T) {
	t.Parallel()

	// 1. Run task
	// 2. Shutdown alloc runner
	// 3. Set alloc.desiredstatus=false
	// 4. Start new alloc runner
	// 5. Assert task and logmon are cleaned up

	alloc := mock.Alloc()
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Driver = "mock_driver"
	task.Config = map[string]interface{}{
		"run_for": "1h",
	}

	conf, cleanup := testAllocRunnerConfig(t, alloc.Copy())
	defer cleanup()

	// Maintain state for subsequent run
	conf.StateDB = state.NewMemDB(conf.Logger)

	// Start and wait for task to be running
	ar, err := NewAllocRunner(conf)
	require.NoError(t, err)
	go ar.Run()
	defer destroy(ar)

	testutil.WaitForResult(func() (bool, error) {
		s := ar.AllocState()
		return s.ClientStatus == structs.AllocClientStatusRunning, fmt.Errorf("expected running, got %s", s.ClientStatus)
	}, func(err error) {
		require.NoError(t, err)
	})

	// Shutdown the AR and manually change the state to mimic a crash where
	// a stopped alloc update is received, but Nomad crashes before
	// stopping the alloc.
	ar.Shutdown()
	select {
	case <-ar.ShutdownCh():
	case <-time.After(30 * time.Second):
		require.Fail(t, "AR took too long to exit")
	}

	// Assert logmon is still running. This is a super ugly hack that pulls
	// logmon's PID out of its reattach config, but it does properly ensure
	// logmon gets cleaned up.
	ls, _, err := conf.StateDB.GetTaskRunnerState(alloc.ID, task.Name)
	require.NoError(t, err)
	require.NotNil(t, ls)

	logmonReattach := struct {
		Pid int
	}{}
	err = json.Unmarshal([]byte(ls.Hooks["logmon"].Data["reattach_config"]), &logmonReattach)
	require.NoError(t, err)

	logmonProc, _ := os.FindProcess(logmonReattach.Pid)
	require.NoError(t, logmonProc.Signal(syscall.Signal(0)))

	// Fake alloc terminal during Restore()
	alloc.DesiredStatus = structs.AllocDesiredStatusStop
	alloc.ModifyIndex++
	alloc.AllocModifyIndex++

	// Start a new alloc runner and assert it gets stopped
	conf2, cleanup2 := testAllocRunnerConfig(t, alloc)
	defer cleanup2()

	// Use original statedb to maintain hook state
	conf2.StateDB = conf.StateDB

	// Restore, start, and wait for task to be killed
	ar2, err := NewAllocRunner(conf2)
	require.NoError(t, err)

	require.NoError(t, ar2.Restore())

	go ar2.Run()
	defer destroy(ar2)

	select {
	case <-ar2.WaitCh():
	case <-time.After(30 * time.Second):
	}

	// Assert logmon was cleaned up
	require.Error(t, logmonProc.Signal(syscall.Signal(0)))

	// Assert consul was cleaned up:
	//   2 removals (canary+noncanary) during prekill
	//   2 removals (canary+noncanary) during exited
	//   2 removals (canary+noncanary) during stop
	consulOps := conf2.Consul.(*consul.MockConsulServiceClient).GetOps()
	require.Len(t, consulOps, 6)
	for _, op := range consulOps {
		require.Equal(t, "remove", op.Op)
	}

	// Assert terminated task event was emitted
	events := ar2.AllocState().TaskStates[task.Name].Events
	require.Len(t, events, 4)
	require.Equal(t, events[0].Type, structs.TaskReceived)
	require.Equal(t, events[1].Type, structs.TaskSetup)
	require.Equal(t, events[2].Type, structs.TaskStarted)
	require.Equal(t, events[3].Type, structs.TaskTerminated)
}
