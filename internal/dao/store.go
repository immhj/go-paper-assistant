package dao

import (
	"context"
	"encoding/json"
	"errors"

	"paperassistant/internal/domain"
)

var (
	ErrNotFound              = errors.New("not found")
	ErrDuplicate             = errors.New("duplicate")
	ErrProjectNotFound       = errors.New("project not found")
	ErrConversationNotFound  = errors.New("conversation not found")
	ErrPaperNotFound         = errors.New("paper not found")
	ErrProjectArchived       = errors.New("project archived")
	ErrConversationArchived  = errors.New("conversation archived")
	ErrProjectPaperNotFound  = errors.New("project paper not found")
	ErrResourceStateConflict = errors.New("resource state conflict")
)

// Store is the persistence seam used by the application module. QQ conversations remain
// user-scoped; every Web operation is scoped by a validated ProjectOwner and stable numbers.
type Store interface {
	Ping(context.Context) error
	Close() error

	UpsertUser(context.Context, string) (domain.User, error)
	GetUserByID(context.Context, uint64) (domain.User, error)
	GetUserByOpenID(context.Context, string) (domain.User, error)

	// QQ-only conversation operations.
	CreateConversation(context.Context, uint64, string, json.RawMessage) (domain.Conversation, error)
	CreateConversationForMessage(context.Context, uint64, string, json.RawMessage, string, string) (domain.Conversation, error)
	ListConversations(context.Context, uint64, int) ([]domain.Conversation, error)
	ListAdminConversations(context.Context, uint64, int) ([]domain.Conversation, error)
	GetActiveConversation(context.Context, uint64) (domain.Conversation, error)
	GetConversation(context.Context, uint64, uint64) (domain.Conversation, error)
	SwitchConversation(context.Context, uint64, uint32) (domain.Conversation, error)
	RenameConversation(context.Context, uint64, uint64, string) error
	ArchiveConversation(context.Context, uint64, uint32) error
	RestoreConversation(context.Context, uint64, uint64) error
	UpdateConversationContext(context.Context, uint64, uint64, json.RawMessage) error

	// Web research projects. Mutations lock and validate the owner/project pointer chain.
	GetActiveProjectContext(context.Context, domain.ProjectOwner) (domain.Project, *domain.Conversation, error)
	EnsureActiveProject(context.Context, domain.ProjectOwner, bool, json.RawMessage) (domain.Project, *domain.Conversation, bool, error)
	ListProjects(context.Context, domain.ProjectOwner, string, int, *domain.PagePosition) ([]domain.Project, *domain.PagePosition, error)
	CreateProject(context.Context, domain.ProjectOwner, string, json.RawMessage) (domain.Project, domain.Conversation, error)
	RenameProject(context.Context, domain.ProjectOwner, uint32, string) (domain.Project, error)
	SelectProject(context.Context, domain.ProjectOwner, uint32) (domain.Project, *domain.Conversation, error)
	ArchiveProject(context.Context, domain.ProjectOwner, uint32) (domain.Project, *domain.Project, *domain.Conversation, error)
	RestoreProject(context.Context, domain.ProjectOwner, uint32) (domain.Project, error)

	ListProjectConversations(context.Context, domain.ProjectOwner, uint32, string, int, *domain.PagePosition) ([]domain.Conversation, *domain.PagePosition, error)
	CreateProjectConversation(context.Context, domain.ProjectOwner, uint32, string, json.RawMessage) (domain.Conversation, error)
	RenameProjectConversation(context.Context, domain.ProjectOwner, uint32, uint32, string) (domain.Conversation, error)
	SelectProjectConversation(context.Context, domain.ProjectOwner, uint32, uint32) (domain.Project, domain.Conversation, error)
	ArchiveProjectConversation(context.Context, domain.ProjectOwner, uint32, uint32) (domain.Conversation, *domain.Conversation, error)
	RestoreProjectConversation(context.Context, domain.ProjectOwner, uint32, uint32) (domain.Conversation, error)
	GetProjectConversation(context.Context, domain.ProjectOwner, uint32, uint32) (domain.Conversation, error)
	SaveProjectSearchResult(context.Context, domain.ProjectOwner, uint32, uint32, json.RawMessage, domain.Message) error
	ListActiveProjectMessages(context.Context, domain.ProjectOwner, int) ([]domain.Message, error)
	InsertProjectMessage(context.Context, domain.ProjectOwner, uint32, uint32, domain.Message) (domain.Message, error)

	ListProjectPapers(context.Context, domain.ProjectOwner, uint32, string, int, *domain.PagePosition) ([]domain.ProjectPaper, *domain.PagePosition, error)
	ListCurrentProjectPapers(context.Context, domain.ProjectOwner, uint32, []uint64, int) ([]domain.ProjectPaper, error)
	AddProjectPaper(context.Context, domain.ProjectOwner, uint32, uint64) (domain.ProjectPaper, bool, error)
	RemoveProjectPaper(context.Context, domain.ProjectOwner, uint32, uint64) error
	ListProjectReadResults(context.Context, domain.ProjectOwner, uint32, uint64, int, *domain.PagePosition) ([]domain.ProjectReadResult, *domain.PagePosition, error)
	CommitProjectReadResult(context.Context, domain.ProjectReadCommit) (domain.ProjectReadResult, error)

	InsertMessage(context.Context, domain.Message) (domain.Message, error)
	SaveSearchResult(context.Context, uint64, uint64, json.RawMessage, domain.Message) error
	HasUserMessage(context.Context, string) (bool, error)
	GetUserMessageByQQID(context.Context, string) (domain.Message, error)
	UpdateMessageContent(context.Context, uint64, string, float64) error
	UpdateMessageDelivery(context.Context, uint64, string, string) error
	FailReservedMessage(context.Context, uint64) error
	UpdateMessageContentAndTrace(context.Context, uint64, string, float64, json.RawMessage) error
	UpdateUserMessageAgentTrace(context.Context, string, json.RawMessage, float64) error
	UpdateMessageAgentTraceByID(context.Context, uint64, json.RawMessage, float64) error
	ListRecentUserMessages(context.Context, uint64, int) ([]domain.Message, error)
	CountBotReplies(context.Context, string) (int, error)
	FindLatestShownMessage(context.Context, uint64) (domain.Message, error)
	FindShownMessageByQQID(context.Context, uint64, string) (domain.Message, error)
	FindRecoverableBotReply(context.Context, string) (domain.Message, error)
	ListConversationMessages(context.Context, uint64, uint64, int) ([]domain.Message, error)
	ListWebConversationMessages(context.Context, uint64, int) ([]domain.Message, error)

	UpsertPaper(context.Context, domain.Paper) (domain.Paper, error)
	GetPaper(context.Context, uint64) (domain.Paper, error)
	UpdatePaperFullText(context.Context, uint64, string, string) error

	// Account-private full text (ADR 0019): keyed by (accountID, paperID), never shared.
	UpsertAccountFullText(ctx context.Context, accountID, paperID uint64, content string) error
	GetAccountFullText(ctx context.Context, accountID, paperID uint64) (string, error)
	GetAccountFullTextStatus(ctx context.Context, accountID, paperID uint64) (domain.PrivateFullTextStatus, error)
	DeleteAccountFullText(ctx context.Context, accountID, paperID uint64) error

	Overview(context.Context, int) (domain.Overview, error)
	ListAdminUsers(context.Context, int) ([]domain.AdminUser, error)
}
