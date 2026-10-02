//go:build integration

package codexcli

// Live checks of what thread/delete removes from CODEX_HOME, against the
// codex on PATH with a signed-in account, in a throwaway CODEX_HOME. They
// spend three small turns, two of which spawn a subagent, and need sqlite3:
//
//	go test -tags integration -run TestLive_DeleteThread -count=1 -v .

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// deleteMarker is what the turn's shell command prints. The prompt holds
// it only in two halves, so finding it whole means tool output was kept.
const deleteMarker = "DELMARK-7731"

const deletePrompt = "Run the shell command `printf 'DELMARK-%s' 7731` and reply with its output and nothing else."

// TestLive_DeleteThread: deleting a persisted thread removes its rollout
// and every SQLite row that mentions it, except codex's tracing log, which
// keeps no tool output. Deleting it again is ErrThreadNotFound.
func TestLive_DeleteThread(t *testing.T) {
	home := deleteHome(t)
	conn := liveMcpConn(t, home,
		WithApprovalPolicy(schema.NewAskForApprovalString("never")),
		WithSandbox(schema.SandboxDangerFull))
	th, err := conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	runTurn(t, th, deletePrompt)
	if n := markerRows(t, home, "thread_history_1.sqlite", "thread_items"); n == 0 {
		t.Fatalf("%s not in thread_items before delete; the command did not run", deleteMarker)
	}
	requireStored(t, threadTraces(t, home, th.ID), th.ID)

	if err := conn.DeleteThread(context.Background(), th.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if err := conn.DeleteThread(context.Background(), th.ID); !errors.Is(err, ErrThreadNotFound) {
		t.Errorf("second DeleteThread = %v, want ErrThreadNotFound", err)
	}
	conn.Close()
	requireDeleted(t, home, th.ID)
}

// TestLive_DeleteThreadSubagent: deleting a parent deletes the subagent
// thread it spawned, whether or not the deleting connection has the
// threads loaded; the child's own delete is then ErrThreadNotFound.
func TestLive_DeleteThreadSubagent(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		name := "same connection"
		if fresh {
			name = "fresh connection"
		}
		t.Run(name, func(t *testing.T) {
			home := deleteHome(t)
			conn := liveMcpConn(t, home,
				WithApprovalPolicy(schema.NewAskForApprovalString("never")),
				WithSandbox(schema.SandboxDangerFull))
			th, err := conn.NewThread(context.Background())
			if err != nil {
				t.Fatalf("NewThread: %v", err)
			}
			runTurn(t, th, "Spawn exactly one subagent. Its task: "+deletePrompt+" Wait for it to finish, then reply with the word done. Do not run the command yourself.")
			children := th.Children()
			if len(children) != 1 {
				t.Fatalf("Children() = %+v, want one subagent", children)
			}
			child := children[0].ThreadID
			requireStored(t, threadTraces(t, home, child), child)
			if fresh {
				conn.Close()
				conn = liveMcpConn(t, home)
			}

			if err := conn.DeleteThread(context.Background(), th.ID); err != nil {
				t.Fatalf("DeleteThread parent: %v", err)
			}
			if err := conn.DeleteThread(context.Background(), child); !errors.Is(err, ErrThreadNotFound) {
				t.Errorf("DeleteThread child after parent = %v, want ErrThreadNotFound", err)
			}
			if _, ok := conn.ChildThread(child); ok && !fresh {
				t.Error("deleted child still reported by ChildThread")
			}
			conn.Close()
			requireDeleted(t, home, th.ID)
			requireDeleted(t, home, child)
		})
	}
}

// requireDeleted fails if anything under home still mentions threadID,
// other than codex's tracing log, and if that log or any other table
// holds the turn's tool output.
func requireDeleted(t *testing.T, home, threadID string) {
	t.Helper()
	var left []string
	for _, tr := range threadTraces(t, home, threadID) {
		if strings.HasPrefix(tr, "logs_2.sqlite:logs=") {
			t.Logf("%s: %s kept (tracing spans, request metadata)", threadID, tr)
			continue
		}
		left = append(left, tr)
	}
	if len(left) > 0 {
		t.Errorf("after delete, %s still in %v", threadID, left)
	}
	for _, db := range []string{"thread_history_1.sqlite", "state_5.sqlite", "logs_2.sqlite"} {
		for _, table := range sqliteLines(t, filepath.Join(home, db), "SELECT name FROM sqlite_master WHERE type='table'") {
			if n := markerRows(t, home, db, table); n > 0 {
				t.Errorf("after delete, %s:%s has %d rows with %s", db, table, n, deleteMarker)
			}
		}
	}
}

func markerRows(t *testing.T, home, db, table string) int {
	t.Helper()
	return tableRowsContaining(t, filepath.Join(home, db), table, deleteMarker)
}

func deleteHome(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	home, signedIn := sandboxCodexHome(t)
	if !signedIn {
		t.Skip("codex home not signed in")
	}
	return home
}

func runTurn(t *testing.T, th *Thread, prompt string) {
	t.Helper()
	stream, err := th.StartTurn(context.Background(), prompt)
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	defer stream.Close()
	turn, err := drainTurnObserving(stream, 3*time.Minute, nil)
	if err != nil || turn.Status != schema.TurnCompleted {
		t.Fatalf("turn = %+v, %v", turn, err)
	}
}

// requireStored fails unless the thread is in each place codex keeps a
// persisted thread: its rollout, its turns and items, and its threads row.
func requireStored(t *testing.T, traces []string, threadID string) {
	t.Helper()
	for _, want := range []string{
		"sessions/rollout-" + threadID,
		"thread_history_1.sqlite:thread_items",
		"thread_history_1.sqlite:thread_turns",
		"state_5.sqlite:threads",
	} {
		found := false
		for _, tr := range traces {
			found = found || strings.HasPrefix(tr, want)
		}
		if !found {
			t.Errorf("%s not in %s; traces %v", threadID, want, traces)
		}
	}
}

// threadTraces lists every place under home that mentions threadID: a
// file that names or contains it (rollouts as "sessions/rollout-<id>"),
// and "db:table=rows" for each table with rows that do, in any column.
// SQLite is opened read-only.
func threadTraces(t *testing.T, home, threadID string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(d.Name(), ".sqlite") || d.Name() == "auth.json" {
			return err
		}
		rel, _ := filepath.Rel(home, path)
		if strings.HasPrefix(rel, "sessions"+string(filepath.Separator)) && strings.Contains(d.Name(), threadID) {
			out = append(out, "sessions/rollout-"+threadID)
			return nil
		}
		body, err := os.ReadFile(path)
		if err == nil && bytes.Contains(body, []byte(threadID)) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", home, err)
	}
	dbs, _ := filepath.Glob(filepath.Join(home, "*.sqlite"))
	for _, db := range dbs {
		for _, table := range sqliteLines(t, db, "SELECT name FROM sqlite_master WHERE type='table'") {
			if n := tableRowsContaining(t, db, table, threadID); n > 0 {
				out = append(out, fmt.Sprintf("%s:%s=%d", filepath.Base(db), table, n))
			}
		}
	}
	sort.Strings(out)
	return out
}

// tableRowsContaining counts the rows of table with s in any column.
func tableRowsContaining(t *testing.T, db, table, s string) int {
	t.Helper()
	var where []string
	for _, c := range sqliteLines(t, db, "SELECT name FROM pragma_table_info('"+table+"')") {
		where = append(where, "instr(CAST(\""+c+"\" AS TEXT), '"+s+"') > 0")
	}
	out := sqliteLines(t, db, "SELECT count(*) FROM \""+table+"\" WHERE "+strings.Join(where, " OR "))
	n, err := strconv.Atoi(strings.Join(out, ""))
	if err != nil {
		t.Fatalf("count %s:%s: %q", filepath.Base(db), table, out)
	}
	return n
}

func sqliteLines(t *testing.T, db, query string) []string {
	t.Helper()
	out, err := exec.Command("sqlite3", "-readonly", db, query).Output()
	if err != nil {
		t.Fatalf("sqlite3 %s %q: %v", filepath.Base(db), query, err)
	}
	return strings.Fields(strings.TrimSpace(string(out)))
}
