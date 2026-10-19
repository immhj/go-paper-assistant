//go:build integration

package mysql_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"paperassistant/internal/config"
	mysqlstore "paperassistant/internal/dao/mysql"
	"paperassistant/internal/domain"
)

// MYSQL_TEST_ADMIN_DSN is deliberately a server-level, test-only credential. The harness creates
// a random schema and never uses MYSQL_DSN, so integration tests cannot erase a running database.
const integrationAdminDSNEnv = "MYSQL_TEST_ADMIN_DSN"

var integration struct {
	admin *sql.DB
	dsn   string
	skip  string
}

func TestMain(m *testing.M) {
	adminDSN := os.Getenv(integrationAdminDSNEnv)
	if adminDSN == "" {
		integration.skip = integrationAdminDSNEnv + " is not set"
		os.Exit(m.Run())
	}
	configValue, err := mysqldriver.ParseDSN(adminDSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse integration MySQL admin DSN:", err)
		os.Exit(1)
	}
	configValue.DBName = ""
	configValue.MultiStatements = true
	configValue.ParseTime = true
	configValue.Loc = time.UTC
	admin, err := sql.Open("mysql", configValue.FormatDSN())
	if err != nil {
		fmt.Fprintln(os.Stderr, "open integration MySQL admin connection:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = admin.PingContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ping integration MySQL:", err)
		_ = admin.Close()
		os.Exit(1)
	}
	integration.admin = admin
	code := m.Run()
	_ = admin.Close()
	os.Exit(code)
}

func requireStore(t *testing.T) (*mysqlstore.Store, *sql.DB) {
	t.Helper()
	if integration.skip != "" {
		t.Skip(integration.skip)
	}
	name := "paper_assistant_it_" + randomHex(t, 8)
	ctx := context.Background()
	if _, err := integration.admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := integration.admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
			t.Errorf("drop isolated schema %s: %v", name, err)
		}
	})

	configValue, err := mysqldriver.ParseDSN(os.Getenv(integrationAdminDSNEnv))
	if err != nil {
		t.Fatalf("parse integration MySQL app DSN: %v", err)
	}
	configValue.DBName = name
	configValue.MultiStatements = true
	configValue.ParseTime = true
	configValue.Loc = time.UTC
	raw, err := sql.Open("mysql", configValue.FormatDSN())
	if err != nil {
		t.Fatalf("open isolated schema: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err = raw.ExecContext(ctx, mustSchema(t)); err != nil {
		t.Fatalf("apply docs/schema.sql: %v", err)
	}

	// Use the production constructor after fresh installation. It verifies that startup's
	// additive schema checks remain compatible and idempotent with the authority schema.
	configValue.MultiStatements = false
	store, err := mysqlstore.Open(ctx, config.MySQLConfig{DSN: configValue.FormatDSN(), ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open production MySQL store against fresh schema: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, raw
}

func mustSchema(t *testing.T) string {
	t.Helper()
	path := repositoryFile(t, "docs", "schema.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func repositoryFile(t *testing.T, parts ...string) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		candidate := filepath.Join(directory, "go.mod")
		if _, err := os.Stat(candidate); err == nil {
			return filepath.Join(append([]string{directory}, parts...)...)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("could not find repository root")
		}
		directory = parent
	}
}

func randomHex(t *testing.T, bytes int) string {
	t.Helper()
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", value)
}

func testAccount(t *testing.T, store *mysqlstore.Store) domain.Account {
	t.Helper()
	suffix := randomHex(t, 6)
	account, _, err := store.CreateAccount(context.Background(), domain.CreateAccountParams{
		Username: "account_" + suffix, Email: "account_" + suffix + "@example.test", Nickname: "integration",
		PasswordHash: "argon2id-test-hash", EmailVerifiedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return account
}

func testGuest(t *testing.T, store *mysqlstore.Store) (domain.GuestSession, []byte) {
	t.Helper()
	hash := make([]byte, 32)
	if _, err := rand.Read(hash); err != nil {
		t.Fatal(err)
	}
	guest, err := store.CreateGuestSession(context.Background(), hash)
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	return guest, hash
}

func accountOwner(account domain.Account) domain.ProjectOwner {
	return domain.ProjectOwner{Type: domain.ProjectOwnerAccount, ID: account.ID}
}

func guestOwner(guest domain.GuestSession) domain.ProjectOwner {
	return domain.ProjectOwner{Type: domain.ProjectOwnerGuest, ID: guest.ID}
}

func TestCommitProjectReadResult_MySQLAtomicGuestFlowAndIdempotency(t *testing.T) {
	store, raw := requireStore(t)
	ctx := context.Background()
	guest, hash := testGuest(t, store)
	owner := guestOwner(guest)
	project, conversation, created, err := store.EnsureActiveProject(ctx, owner, true, []byte(`{"name":"test"}`))
	if err != nil || !created || conversation == nil {
		t.Fatalf("ensure guest active project: project=%+v conversation=%+v created=%t err=%v", project, conversation, created, err)
	}
	reservationID := "read-" + randomHex(t, 8)
	if _, err = store.ReserveGuestDeepRead(ctx, guest.ID, reservationID); err != nil {
		t.Fatalf("reserve guest read: %v", err)
	}
	paper, err := store.UpsertPaper(ctx, domain.Paper{Source: domain.PaperSourceArxiv, ExternalID: "2401." + randomHex(t, 4), Title: "Atomic project read", FullTextStatus: "not_fetched"})
	if err != nil {
		t.Fatalf("upsert paper: %v", err)
	}
	message, err := store.InsertProjectMessage(ctx, owner, project.ProjectNo, conversation.ConversationNo, domain.Message{Role: "bot"})
	if err != nil {
		t.Fatalf("insert reserved bot message: %v", err)
	}
	commit := domain.ProjectReadCommit{ProjectOwner: owner, ProjectNo: project.ProjectNo, ConversationNo: conversation.ConversationNo,
		PaperID: paper.ID, MessageID: message.ID, ReadType: domain.ProjectReadTypeDeep, Content: "grounded read", CostUSD: 0.012, GuestReservationID: &reservationID}
	result, err := store.CommitProjectReadResult(ctx, commit)
	if err != nil {
		t.Fatalf("commit project read: %v", err)
	}
	if result.MessageID != message.ID || result.ReadType != domain.ProjectReadTypeDeep || result.Content != commit.Content {
		t.Fatalf("unexpected committed read: %+v", result)
	}
	papers, err := store.ListCurrentProjectPapers(ctx, owner, project.ProjectNo, []uint64{paper.ID}, 10)
	if err != nil || len(papers) != 1 || papers[0].PaperID != paper.ID || papers[0].Status != domain.ProjectPaperStatusActive {
		t.Fatalf("automatic project-paper inclusion failed: papers=%+v err=%v", papers, err)
	}
	history, _, err := store.ListProjectReadResults(ctx, owner, project.ProjectNo, paper.ID, 10, nil)
	if err != nil || len(history) != 1 || history[0].ID != result.ID {
		t.Fatalf("read history: history=%+v err=%v", history, err)
	}
	var delivery, content string
	var cost float64
	if err = raw.QueryRowContext(ctx, "SELECT delivery_status,COALESCE(content,''),cost_usd FROM messages WHERE id=?", message.ID).Scan(&delivery, &content, &cost); err != nil {
		t.Fatal(err)
	}
	if delivery != "sent" || content != commit.Content || cost != commit.CostUSD {
		t.Fatalf("message was not atomically completed: delivery=%q content=%q cost=%v", delivery, content, cost)
	}
	consumed, err := store.GetGuestSessionByTokenHash(ctx, hash)
	if err != nil || consumed.DeepReadState != domain.GuestDeepReadConsumed || consumed.ReservationID == nil || *consumed.ReservationID != reservationID {
		t.Fatalf("guest reservation was not consumed: guest=%+v err=%v", consumed, err)
	}
	retry, err := store.CommitProjectReadResult(ctx, commit)
	if err != nil || retry.ID != result.ID {
		t.Fatalf("idempotent read retry: result=%+v err=%v", retry, err)
	}
	var count int
	if err = raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM project_read_results WHERE message_id=?", message.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry duplicated read result: count=%d err=%v", count, err)
	}
}

func TestImportGuestSession_MySQLReassignsNumbersAndReplays(t *testing.T) {
	store, raw := requireStore(t)
	ctx := context.Background()
	account := testAccount(t, store)
	accountOwner := accountOwner(account)
	if _, _, err := store.CreateProject(ctx, accountOwner, "existing account project", nil); err != nil {
		t.Fatalf("create account project: %v", err)
	}
	guest, hash := testGuest(t, store)
	guestOwner := guestOwner(guest)
	first, _, err := store.CreateProject(ctx, guestOwner, "guest #1", nil)
	if err != nil {
		t.Fatalf("create first guest project: %v", err)
	}
	if _, _, err = store.CreateProject(ctx, guestOwner, "guest #2", nil); err != nil {
		t.Fatalf("create second guest project: %v", err)
	}
	if _, _, err = store.SelectProject(ctx, guestOwner, first.ProjectNo); err != nil {
		t.Fatalf("select first guest project: %v", err)
	}
	key := "import-" + randomHex(t, 8)
	imported, err := store.ImportGuestSession(ctx, account.ID, hash, key)
	if err != nil {
		t.Fatalf("import guest: %v", err)
	}
	if !imported.Upgraded || imported.AlreadyImported || !equalNos(imported.ImportedProjectNos, []uint32{3, 2}) || imported.CurrentProjectNo == nil || *imported.CurrentProjectNo != 3 {
		t.Fatalf("unexpected import result: %+v", imported)
	}
	selection, err := store.GetResearchSelection(ctx, accountOwner)
	if err != nil || selection.ActiveProjectNo == nil || *selection.ActiveProjectNo != 3 {
		t.Fatalf("account selection after import: %+v err=%v", selection, err)
	}
	var accountOwned, guestOwned int
	if err = raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM projects WHERE owner_account_id=?", account.ID).Scan(&accountOwned); err != nil {
		t.Fatal(err)
	}
	if err = raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM projects WHERE owner_guest_id=?", guest.ID).Scan(&guestOwned); err != nil {
		t.Fatal(err)
	}
	if accountOwned != 3 || guestOwned != 0 {
		t.Fatalf("ownership transfer failed: account=%d guest=%d", accountOwned, guestOwned)
	}
	replay, err := store.ImportGuestSession(ctx, account.ID, hash, key)
	if err != nil || !replay.Upgraded || !replay.AlreadyImported || !equalNos(replay.ImportedProjectNos, imported.ImportedProjectNos) {
		t.Fatalf("import replay: result=%+v err=%v", replay, err)
	}
	if err = raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM projects WHERE owner_account_id=?", account.ID).Scan(&accountOwned); err != nil || accountOwned != 3 {
		t.Fatalf("replay changed ownership: count=%d err=%v", accountOwned, err)
	}
}

func TestCreateProjectAndConversation_MySQLSerializesOwnerScopedNumbers(t *testing.T) {
	store, _ := requireStore(t)
	account := testAccount(t, store)
	owner := accountOwner(account)
	const workers = 8
	start := make(chan struct{})
	projects := make(chan domain.Project, workers)
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			<-start
			project, conversation, err := store.CreateProject(context.Background(), owner, fmt.Sprintf("project %d", i), nil)
			if err == nil && conversation.ConversationNo != 1 {
				err = fmt.Errorf("project %d default conversation number=%d", project.ProjectNo, conversation.ConversationNo)
			}
			if err != nil {
				errs <- err
				return
			}
			projects <- project
		}(i)
	}
	close(start)
	wait.Wait()
	close(projects)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent project creation: %v", err)
	}
	projectNos := collectProjectNos(projects)
	if !equalNos(projectNos, []uint32{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("project numbers=%v", projectNos)
	}

	project := mustProject(t, store, owner)
	start = make(chan struct{})
	conversations := make(chan domain.Conversation, workers)
	errs = make(chan error, workers)
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			<-start
			conversation, err := store.CreateProjectConversation(context.Background(), owner, project.ProjectNo, fmt.Sprintf("conversation %d", i), nil)
			if err != nil {
				errs <- err
				return
			}
			conversations <- conversation
		}(i)
	}
	close(start)
	wait.Wait()
	close(conversations)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent conversation creation: %v", err)
	}
	nos := make([]uint32, 0, workers)
	for conversation := range conversations {
		nos = append(nos, conversation.ConversationNo)
	}
	sort.Slice(nos, func(i, j int) bool { return nos[i] < nos[j] })
	if !equalNos(nos, []uint32{2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Fatalf("conversation numbers=%v", nos)
	}
}

func mustProject(t *testing.T, store *mysqlstore.Store, owner domain.ProjectOwner) domain.Project {
	t.Helper()
	projects, _, err := store.ListProjects(context.Background(), owner, domain.ProjectStatusActive, 100, nil)
	if err != nil || len(projects) == 0 {
		t.Fatalf("list projects: projects=%+v err=%v", projects, err)
	}
	return projects[0]
}

func collectProjectNos(items <-chan domain.Project) []uint32 {
	nos := make([]uint32, 0)
	for item := range items {
		nos = append(nos, item.ProjectNo)
	}
	sort.Slice(nos, func(i, j int) bool { return nos[i] < nos[j] })
	return nos
}

func equalNos(actual, expected []uint32) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}
