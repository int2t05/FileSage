//go:build linux

// 本文件验证 Agent 计划在独立服务进程中断后可安全恢复。
package agent_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/agent"
	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/httpapi"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
)

const (
	recoveryHelperEnv        = "FILESAGE_TEST_AGENT_RECOVERY_HELPER"
	recoveryFixtureFileCount = 20_000
)

type recoveryPlanResponse struct {
	Status     string `json:"status"`
	Operations []struct {
		ID     string `json:"id"`
		Status string `json:"executionStatus"`
	} `json:"operations"`
}

func TestAgentPlanRecoversAcrossProcessRestartWithoutReplay(t *testing.T) {
	ctx := context.Background()
	rootPath, dataPath := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < recoveryFixtureFileCount; index++ {
		name := filepath.Join(rootPath, "source", fmt.Sprintf("file-%05d.txt", index))
		if err := os.WriteFile(name, []byte("recovery fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	databasePath := filepath.Join(dataPath, "filesage.sqlite")
	metadata, root, _, service := openRecoveryRuntime(t, databasePath, rootPath)
	if err := metadata.SetAgentBypassPermission(ctx, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	plan, err := service.Propose(ctx, agent.Proposal{
		RequestText: "复制 recovery fixture 文件夹", Action: "copy_directory",
		SourcePath: "source", TargetPath: "destination/copy", Reason: "验证进程恢复",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = service.StartPlan(ctx, plan.ID, plan.Version, "manual", false)
	if err != nil || len(plan.Operations) != 1 {
		t.Fatalf("StartPlan() = (%#v, %v)", plan, err)
	}
	operationID, planVersion := plan.Operations[0].ID, plan.Version
	closeRecoveryRuntime(t, metadata, root)

	process, baseURL := startRecoveryProcess(t, databasePath, rootPath)
	defer killRecoveryProcess(t, process)
	waitRecoveryHealth(t, process, baseURL)
	executionDone := make(chan error, 1)
	go func() {
		body := strings.NewReader(fmt.Sprintf(`{"expectedPlanVersion":%d}`, planVersion))
		request, requestErr := http.NewRequest(http.MethodPost,
			baseURL+"/api/v1/agent/plans/"+plan.ID+"/steps/"+operationID+"/execute", body)
		if requestErr != nil {
			executionDone <- requestErr
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", baseURL)
		response, requestErr := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		executionDone <- requestErr
	}()

	started := waitForRunningCopyStaging(t, process, baseURL, plan.ID, filepath.Join(rootPath, "destination"))
	if len(started.Operations) != 1 || started.Operations[0].Status != string(store.OperationRunning) {
		t.Fatalf("copy did not enter running state: %#v", started)
	}
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = process.Wait()
	<-executionDone

	metadata, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := metadata.Plan(ctx, plan.ID)
	if err != nil || persisted.Status != store.PlanExecuting || persisted.Operations[0].Status != store.OperationRunning {
		_ = metadata.Close()
		t.Fatalf("killed process did not leave an in-flight operation for recovery: plan=%#v err=%v", persisted, err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, restartURL := startRecoveryProcess(t, databasePath, rootPath)
	defer killRecoveryProcess(t, restarted)
	waitRecoveryHealth(t, restarted, restartURL)
	recovered := getRecoveryPlan(t, restartURL, plan.ID)
	if recovered.Status == string(store.PlanExecuting) || len(recovered.Operations) != 1 || recovered.Operations[0].Status == string(store.OperationRunning) {
		t.Fatalf("restart left the interrupted operation running: %#v", recovered)
	}
	if recovered.Status != string(store.PlanApproved) || recovered.Operations[0].Status != string(store.OperationPending) {
		t.Fatalf("interrupted copy should be ready for explicit retry: %#v", recovered)
	}
	if hasTreeCopyStaging(t, filepath.Join(rootPath, "destination")) {
		t.Fatal("recovery left an interrupted copy staging entry")
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "destination", "copy")); !os.IsNotExist(err) {
		t.Fatalf("recovery replayed the copy or left a published target: %v", err)
	}
}

// TestAgentRecoveryProcessHelper 在独立子进程中启动真实 Agent API。
func TestAgentRecoveryProcessHelper(t *testing.T) {
	if os.Getenv(recoveryHelperEnv) != "1" {
		return
	}
	databasePath, rootPath := os.Getenv("FILESAGE_TEST_AGENT_RECOVERY_DB"), os.Getenv("FILESAGE_TEST_AGENT_RECOVERY_ROOT")
	metadata, root, trash, service := openRecoveryRuntime(t, databasePath, rootPath)
	if err := service.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler := httpapi.NewWithAgentAndIndex(root, metadata, trash, service, nil, metadata.Health, coordination.New())
	server := httptest.NewServer(handler)
	defer server.Close()
	if _, err := fmt.Fprintln(os.Stdout, server.URL); err != nil {
		t.Fatal(err)
	}
	select {}
}

// openRecoveryRuntime 创建与服务一致的隔离运行依赖。
func openRecoveryRuntime(t *testing.T, databasePath, rootPath string) (*store.Store, *fsroot.Root, *trashsvc.Manager, *agent.Service) {
	t.Helper()
	metadata, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	if err := root.ConfigureMoveRecoveryDirectory(filepath.Join(filepath.Dir(databasePath), "move-recovery")); err != nil {
		_ = root.Close()
		_ = metadata.Close()
		t.Fatal(err)
	}
	trash, err := trashsvc.Open(context.Background(), root, metadata)
	if err != nil {
		_ = root.Close()
		_ = metadata.Close()
		t.Fatal(err)
	}
	return metadata, root, trash, agent.New(metadata, root, coordination.New(), trash)
}

// closeRecoveryRuntime 按依赖顺序关闭测试运行时。
func closeRecoveryRuntime(t *testing.T, metadata *store.Store, root *fsroot.Root) {
	t.Helper()
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
}

// startRecoveryProcess 启动隔离 API 进程并返回服务地址。
func startRecoveryProcess(t *testing.T, databasePath, rootPath string) (*exec.Cmd, string) {
	t.Helper()
	process := exec.Command(os.Args[0], "-test.run=^TestAgentRecoveryProcessHelper$")
	process.Env = append(os.Environ(), recoveryHelperEnv+"=1",
		"FILESAGE_TEST_AGENT_RECOVERY_DB="+databasePath,
		"FILESAGE_TEST_AGENT_RECOVERY_ROOT="+rootPath,
	)
	stdout, err := process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	process.Stderr = os.Stderr
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = process.Wait()
		t.Fatalf("Agent process did not report its API address: %v", err)
	}
	return process, strings.TrimSpace(line)
}

// waitRecoveryHealth 等待子进程健康检查通过。
func waitRecoveryHealth(t *testing.T, process *exec.Cmd, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		response, err := client.Get(baseURL + "/api/v1/health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if process.ProcessState != nil {
			t.Fatalf("Agent process exited before health check: %v", process.ProcessState)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Agent API did not become healthy")
}

// waitForRunningCopyStaging 等待运行步骤写入部分暂存文件。
func waitForRunningCopyStaging(t *testing.T, process *exec.Cmd, baseURL, planID, destinationParent string) recoveryPlanResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		plan := getRecoveryPlan(t, baseURL, planID)
		if len(plan.Operations) == 1 && plan.Operations[0].Status == string(store.OperationRunning) && hasPartialTreeCopy(t, destinationParent) {
			return plan
		}
		if process.ProcessState != nil || plan.Status != string(store.PlanApproved) && plan.Status != string(store.PlanExecuting) {
			t.Fatalf("copy completed before a staged file could be observed: %#v", plan)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("Agent copy did not expose a partial staged tree in time")
	return recoveryPlanResponse{}
}

// hasPartialTreeCopy 检查暂存目录中存在未完成的文件列表。
func hasPartialTreeCopy(t *testing.T, destinationParent string) bool {
	t.Helper()
	entries, err := os.ReadDir(destinationParent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".filesage-tree-copy-") {
			continue
		}
		staged, err := os.ReadDir(filepath.Join(destinationParent, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		copiedFiles := 0
		for _, file := range staged {
			if strings.HasPrefix(file.Name(), "file-") {
				copiedFiles++
			}
		}
		if copiedFiles > 0 && copiedFiles < recoveryFixtureFileCount {
			return true
		}
	}
	return false
}

// hasTreeCopyStaging 检查目录复制暂存项是否残留。
func hasTreeCopyStaging(t *testing.T, destinationParent string) bool {
	t.Helper()
	entries, err := os.ReadDir(destinationParent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".filesage-tree-copy-") {
			return true
		}
	}
	return false
}

// getRecoveryPlan 读取服务端持久计划状态。
func getRecoveryPlan(t *testing.T, baseURL, planID string) recoveryPlanResponse {
	t.Helper()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(baseURL + "/api/v1/agent/plans/" + planID)
	if err != nil {
		t.Fatalf("GET agent plan: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET agent plan status = %d", response.StatusCode)
	}
	var plan recoveryPlanResponse
	if err := json.NewDecoder(response.Body).Decode(&plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

// killRecoveryProcess 确保隔离子进程退出。
func killRecoveryProcess(t *testing.T, process *exec.Cmd) {
	t.Helper()
	if process.ProcessState != nil {
		return
	}
	if err := process.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			if waitErr := process.Wait(); waitErr != nil {
				t.Errorf("Agent process exited before cleanup: %v", waitErr)
			} else {
				t.Error("Agent process exited before cleanup")
			}
			return
		}
		t.Errorf("terminate Agent process: %v", err)
		return
	}
	// 子进程被终止后，Wait 返回退出错误是预期结果。
	if err := process.Wait(); err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != -1 {
			t.Errorf("Agent process did not stop from the requested termination: %v", err)
		}
	} else {
		t.Error("Agent process exited cleanly after termination was requested")
	}
}
