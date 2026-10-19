package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

var cursorKey = func() []byte {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		sum := sha256.Sum256([]byte(time.Now().String()))
		return sum[:]
	}
	return value
}()

type ListPage[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

type ProjectConversation struct {
	ConversationNo uint32    `json:"conversationNo"`
	Title          string    `json:"title"`
	Status         string    `json:"status"`
	Selected       bool      `json:"selected"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

func projectConversation(value domain.Conversation, selected bool) ProjectConversation {
	return ProjectConversation{ConversationNo: value.ConversationNo, Title: value.Title, Status: value.Status, Selected: selected, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}

type cursorPayload struct {
	OwnerType string    `json:"o"`
	OwnerID   uint64    `json:"i"`
	Endpoint  string    `json:"e"`
	Parent    uint32    `json:"p"`
	Status    string    `json:"s"`
	PaperID   uint64    `json:"r"`
	Time      time.Time `json:"t"`
	ID        uint64    `json:"d"`
}

func projectOwner(actor WebActor) (domain.ProjectOwner, error) {
	actor, err := normalizeWebActor(actor)
	if err != nil {
		return domain.ProjectOwner{}, err
	}
	return domain.NewProjectOwner(actor.Type, actor.ID)
}
func encodeCursor(payload cursorPayload) *string {
	raw, _ := json.Marshal(payload)
	block, _ := aes.NewCipher(cursorKey)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil
	}
	sealed := aead.Seal(nonce, nonce, raw, nil)
	token := base64.RawURLEncoding.EncodeToString(sealed)
	return &token
}
func decodeCursor(token string, expected cursorPayload) (*domain.PagePosition, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	block, _ := aes.NewCipher(cursorKey)
	aead, _ := cipher.NewGCM(block)
	if len(sealed) < aead.NonceSize() {
		return nil, ErrInvalidCursor
	}
	raw, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var value cursorPayload
	if json.Unmarshal(raw, &value) != nil || value.OwnerType != expected.OwnerType || value.OwnerID != expected.OwnerID || value.Endpoint != expected.Endpoint || value.Parent != expected.Parent || value.Status != expected.Status || value.PaperID != expected.PaperID || value.Time.IsZero() || value.ID == 0 {
		return nil, ErrInvalidCursor
	}
	return &domain.PagePosition{Time: value.Time, ID: value.ID}, nil
}

var ErrInvalidCursor = errors.New("invalid cursor")

func validStatus(value string, allowed ...string) (string, error) {
	if value == "" {
		return allowed[0], nil
	}
	for _, item := range allowed {
		if value == item {
			return value, nil
		}
	}
	return "", ErrValidation
}
func validTitle(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len([]rune(value)) > 128 {
		return "", ErrValidation
	}
	return value, nil
}
func clampPageLimit(limit int) int {
	if limit == 0 {
		return 20
	}
	if limit < 1 || limit > 100 {
		return -1
	}
	return limit
}
func defaultConversationContext() json.RawMessage {
	raw, _ := json.Marshal(newSearchContext("默认会话"))
	return raw
}

func (a *Application) EnsureActiveProject(ctx context.Context, actor WebActor) (domain.Project, *ProjectConversation, bool, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return domain.Project{}, nil, false, err
	}
	project, conversation, created, err := a.store.EnsureActiveProject(ctx, owner, false, defaultConversationContext())
	if err != nil {
		return domain.Project{}, nil, false, err
	}
	return project, projectConversationPointer(conversation), created, nil
}
func (a *Application) ensureActiveChatContext(ctx context.Context, actor WebActor) (domain.Project, domain.Conversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	project, conversation, _, err := a.store.EnsureActiveProject(ctx, owner, true, defaultConversationContext())
	if err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	if conversation == nil {
		return domain.Project{}, domain.Conversation{}, dao.ErrResourceStateConflict
	}
	return project, *conversation, nil
}

func (a *Application) ListProjects(ctx context.Context, actor WebActor, status string, limit int, cursor string) (ListPage[domain.Project], error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return ListPage[domain.Project]{}, err
	}
	status, err = validStatus(status, domain.ProjectStatusActive, domain.ProjectStatusArchived)
	if err != nil {
		return ListPage[domain.Project]{}, err
	}
	limit = clampPageLimit(limit)
	if limit < 0 {
		return ListPage[domain.Project]{}, ErrValidation
	}
	binding := cursorPayload{OwnerType: owner.Type, OwnerID: owner.ID, Endpoint: "projects", Status: status}
	position, err := decodeCursor(cursor, binding)
	if err != nil {
		return ListPage[domain.Project]{}, err
	}
	items, next, err := a.store.ListProjects(ctx, owner, status, limit, position)
	if err != nil {
		return ListPage[domain.Project]{}, err
	}
	active, _, activeErr := a.store.GetActiveProjectContext(ctx, owner)
	if activeErr != nil && !errors.Is(activeErr, dao.ErrNotFound) {
		return ListPage[domain.Project]{}, activeErr
	}
	for i := range items {
		items[i].Selected = activeErr == nil && items[i].ID == active.ID
	}
	var nextCursor *string
	if next != nil {
		binding.Time, binding.ID = next.Time, next.ID
		nextCursor = encodeCursor(binding)
	}
	return ListPage[domain.Project]{Items: items, NextCursor: nextCursor}, nil
}
func (a *Application) CreateProject(ctx context.Context, actor WebActor, title string) (domain.Project, ProjectConversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return domain.Project{}, ProjectConversation{}, err
	}
	title, err = validTitle(title)
	if err != nil {
		return domain.Project{}, ProjectConversation{}, err
	}
	project, conversation, err := a.store.CreateProject(ctx, owner, title, defaultConversationContext())
	return project, projectConversation(conversation, true), err
}
func (a *Application) RenameProject(ctx context.Context, actor WebActor, no uint32, title string) (domain.Project, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return domain.Project{}, err
	}
	title, err = validTitle(title)
	if err != nil {
		return domain.Project{}, err
	}
	project, err := a.store.RenameProject(ctx, owner, no, title)
	if err != nil {
		return domain.Project{}, err
	}
	active, _, activeErr := a.store.GetActiveProjectContext(ctx, owner)
	if activeErr == nil {
		project.Selected = active.ID == project.ID
	} else if !errors.Is(activeErr, dao.ErrNotFound) {
		return domain.Project{}, activeErr
	}
	return project, nil
}
func (a *Application) SelectProject(ctx context.Context, actor WebActor, no uint32) (domain.Project, *ProjectConversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return domain.Project{}, nil, err
	}
	project, conversation, err := a.store.SelectProject(ctx, owner, no)
	return project, projectConversationPointer(conversation), err
}
func (a *Application) ArchiveProject(ctx context.Context, actor WebActor, no uint32) (domain.Project, *domain.Project, *ProjectConversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return domain.Project{}, nil, nil, err
	}
	archived, active, conversation, err := a.store.ArchiveProject(ctx, owner, no)
	return archived, active, projectConversationPointer(conversation), err
}
func (a *Application) RestoreProject(ctx context.Context, actor WebActor, no uint32) (domain.Project, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return domain.Project{}, err
	}
	return a.store.RestoreProject(ctx, owner, no)
}

func (a *Application) ListProjectConversations(ctx context.Context, actor WebActor, projectNo uint32, status string, limit int, cursor string) (ListPage[ProjectConversation], error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return ListPage[ProjectConversation]{}, err
	}
	status, err = validStatus(status, domain.ConversationStatusActive, domain.ConversationStatusArchived)
	if err != nil {
		return ListPage[ProjectConversation]{}, err
	}
	limit = clampPageLimit(limit)
	if limit < 0 {
		return ListPage[ProjectConversation]{}, ErrValidation
	}
	binding := cursorPayload{OwnerType: owner.Type, OwnerID: owner.ID, Endpoint: "conversations", Parent: projectNo, Status: status}
	position, err := decodeCursor(cursor, binding)
	if err != nil {
		return ListPage[ProjectConversation]{}, err
	}
	items, next, err := a.store.ListProjectConversations(ctx, owner, projectNo, status, limit, position)
	if err != nil {
		return ListPage[ProjectConversation]{}, err
	}
	activeProject, activeConversation, activeErr := a.store.GetActiveProjectContext(ctx, owner)
	if activeErr != nil && !errors.Is(activeErr, dao.ErrNotFound) {
		return ListPage[ProjectConversation]{}, activeErr
	}
	result := make([]ProjectConversation, 0, len(items))
	for _, item := range items {
		selected := activeErr == nil && activeConversation != nil && activeProject.ProjectNo == projectNo && activeConversation.ID == item.ID
		result = append(result, projectConversation(item, selected))
	}
	var nextCursor *string
	if next != nil {
		binding.Time, binding.ID = next.Time, next.ID
		nextCursor = encodeCursor(binding)
	}
	return ListPage[ProjectConversation]{Items: result, NextCursor: nextCursor}, nil
}
func (a *Application) CreateProjectConversation(ctx context.Context, actor WebActor, projectNo uint32, title string) (ProjectConversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return ProjectConversation{}, err
	}
	title, err = validTitle(title)
	if err != nil {
		return ProjectConversation{}, err
	}
	conversation, err := a.store.CreateProjectConversation(ctx, owner, projectNo, title, defaultConversationContext())
	return projectConversation(conversation, true), err
}
func (a *Application) RenameProjectConversation(ctx context.Context, actor WebActor, projectNo, conversationNo uint32, title string) (ProjectConversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return ProjectConversation{}, err
	}
	title, err = validTitle(title)
	if err != nil {
		return ProjectConversation{}, err
	}
	conversation, err := a.store.RenameProjectConversation(ctx, owner, projectNo, conversationNo, title)
	if err != nil {
		return ProjectConversation{}, err
	}
	activeProject, activeConversation, activeErr := a.store.GetActiveProjectContext(ctx, owner)
	if activeErr != nil && !errors.Is(activeErr, dao.ErrNotFound) {
		return ProjectConversation{}, activeErr
	}
	selected := activeErr == nil && activeConversation != nil && activeProject.ProjectNo == projectNo && activeConversation.ID == conversation.ID
	return projectConversation(conversation, selected), nil
}
func (a *Application) SelectProjectConversation(ctx context.Context, actor WebActor, projectNo, conversationNo uint32) (domain.Project, ProjectConversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return domain.Project{}, ProjectConversation{}, err
	}
	project, conversation, err := a.store.SelectProjectConversation(ctx, owner, projectNo, conversationNo)
	return project, projectConversation(conversation, true), err
}
func (a *Application) ArchiveProjectConversation(ctx context.Context, actor WebActor, projectNo, conversationNo uint32) (ProjectConversation, *ProjectConversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return ProjectConversation{}, nil, err
	}
	archived, active, err := a.store.ArchiveProjectConversation(ctx, owner, projectNo, conversationNo)
	return projectConversation(archived, false), projectConversationPointer(active), err
}
func (a *Application) RestoreProjectConversation(ctx context.Context, actor WebActor, projectNo, conversationNo uint32) (ProjectConversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return ProjectConversation{}, err
	}
	conversation, err := a.store.RestoreProjectConversation(ctx, owner, projectNo, conversationNo)
	return projectConversation(conversation, conversation.Selected), err
}

func (a *Application) ListProjectPapers(ctx context.Context, actor WebActor, projectNo uint32, status string, limit int, cursor string) (ListPage[domain.ProjectPaper], error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return ListPage[domain.ProjectPaper]{}, err
	}
	status, err = validStatus(status, domain.ProjectPaperStatusActive, domain.ProjectPaperStatusRemoved)
	if err != nil {
		return ListPage[domain.ProjectPaper]{}, err
	}
	limit = clampPageLimit(limit)
	if limit < 0 {
		return ListPage[domain.ProjectPaper]{}, ErrValidation
	}
	binding := cursorPayload{OwnerType: owner.Type, OwnerID: owner.ID, Endpoint: "papers", Parent: projectNo, Status: status}
	position, err := decodeCursor(cursor, binding)
	if err != nil {
		return ListPage[domain.ProjectPaper]{}, err
	}
	items, next, err := a.store.ListProjectPapers(ctx, owner, projectNo, status, limit, position)
	if err != nil {
		return ListPage[domain.ProjectPaper]{}, err
	}
	var nextCursor *string
	if next != nil {
		binding.Time, binding.ID = next.Time, next.ID
		nextCursor = encodeCursor(binding)
	}
	return ListPage[domain.ProjectPaper]{Items: items, NextCursor: nextCursor}, nil
}
func (a *Application) AddProjectPaper(ctx context.Context, actor WebActor, projectNo uint32, paperID uint64) (domain.ProjectPaper, bool, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return domain.ProjectPaper{}, false, err
	}
	return a.store.AddProjectPaper(ctx, owner, projectNo, paperID)
}
func (a *Application) RemoveProjectPaper(ctx context.Context, actor WebActor, projectNo uint32, paperID uint64) error {
	owner, err := projectOwner(actor)
	if err != nil {
		return err
	}
	return a.store.RemoveProjectPaper(ctx, owner, projectNo, paperID)
}
func (a *Application) ListProjectReadResults(ctx context.Context, actor WebActor, projectNo uint32, paperID uint64, limit int, cursor string) (ListPage[domain.ProjectReadResult], error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return ListPage[domain.ProjectReadResult]{}, err
	}
	limit = clampPageLimit(limit)
	if limit < 0 {
		return ListPage[domain.ProjectReadResult]{}, ErrValidation
	}
	binding := cursorPayload{OwnerType: owner.Type, OwnerID: owner.ID, Endpoint: "read-results", Parent: projectNo, PaperID: paperID}
	position, err := decodeCursor(cursor, binding)
	if err != nil {
		return ListPage[domain.ProjectReadResult]{}, err
	}
	items, next, err := a.store.ListProjectReadResults(ctx, owner, projectNo, paperID, limit, position)
	if err != nil {
		return ListPage[domain.ProjectReadResult]{}, err
	}
	var nextCursor *string
	if next != nil {
		binding.Time, binding.ID = next.Time, next.ID
		nextCursor = encodeCursor(binding)
	}
	return ListPage[domain.ProjectReadResult]{Items: items, NextCursor: nextCursor}, nil
}
func projectConversationPointer(value *domain.Conversation) *ProjectConversation {
	if value == nil {
		return nil
	}
	item := projectConversation(*value, value.Selected)
	return &item
}
