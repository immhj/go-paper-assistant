package domain

import (
	"errors"
	"strings"
	"time"
)

const (
	ProjectOwnerAccount = "account"
	ProjectOwnerGuest   = "guest"

	ProjectStatusActive   = "active"
	ProjectStatusArchived = "archived"

	ConversationStatusActive   = "active"
	ConversationStatusArchived = "archived"

	ProjectPaperStatusActive  = "active"
	ProjectPaperStatusRemoved = "removed"
)

// ProjectOwner is the validated persistence identity for a Web actor. HTTP clients never
// provide it; services derive it from the authenticated account or guest context.
type ProjectOwner struct {
	Type string
	ID   uint64
}

func NewProjectOwner(ownerType string, id uint64) (ProjectOwner, error) {
	owner := ProjectOwner{Type: strings.ToLower(strings.TrimSpace(ownerType)), ID: id}
	if err := owner.Validate(); err != nil {
		return ProjectOwner{}, err
	}
	return owner, nil
}

func (o ProjectOwner) Validate() error {
	if o.ID == 0 || (o.Type != ProjectOwnerAccount && o.Type != ProjectOwnerGuest) {
		return errors.New("invalid project owner")
	}
	return nil
}

func (o ProjectOwner) IsAccount() bool { return o.Type == ProjectOwnerAccount }
func (o ProjectOwner) IsGuest() bool   { return o.Type == ProjectOwnerGuest }

// Project is an account- or guest-owned research boundary. Internal IDs and owner columns are
// intentionally hidden from JSON; API routes use ProjectNo inside the authenticated owner scope.
type Project struct {
	ID                   uint64    `json:"-"`
	OwnerAccountID       *uint64   `json:"-"`
	OwnerGuestID         *uint64   `json:"-"`
	ProjectNo            uint32    `json:"projectNo"`
	Title                string    `json:"title"`
	ActiveConversationID *uint64   `json:"-"`
	Status               string    `json:"status"`
	Selected             bool      `json:"selected"`
	ActiveConversationNo *uint32   `json:"activeConversationNo"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

func (p Project) ValidateOwner() error {
	if (p.OwnerAccountID == nil) == (p.OwnerGuestID == nil) {
		return errors.New("project must have exactly one owner")
	}
	return nil
}

// PagePosition is the decoded keyset position. The service signs and binds its public cursor;
// DAO implementations only receive the final ordering key.
type PagePosition struct {
	Time time.Time
	ID   uint64
}

type ReadResultMeta struct {
	ReadType             string    `json:"readType"`
	SourceConversationNo uint32    `json:"sourceConversationNo"`
	CreatedAt            time.Time `json:"createdAt"`
}

type ProjectPaper struct {
	ID               uint64          `json:"-"`
	ProjectID        uint64          `json:"-"`
	PaperID          uint64          `json:"paperId"`
	Title            string          `json:"title"`
	PublicationDate  *time.Time      `json:"publicationDate"`
	VenueRaw         *string         `json:"venueRaw"`
	Status           string          `json:"status"`
	LatestReadResult *ReadResultMeta `json:"latestReadResult"`
	CreatedAt        time.Time       `json:"-"`
	UpdatedAt        time.Time       `json:"updatedAt"`
}

const (
	ProjectReadTypeQuick = "quick"
	ProjectReadTypeDeep  = "deep"
)

// ProjectReadCommit is the complete input to the short project-read transaction. Owner and
// stable numbers are derived by the Web service; clients never submit internal ownership IDs.
type ProjectReadCommit struct {
	ProjectOwner       ProjectOwner
	ProjectNo          uint32
	ConversationNo     uint32
	PaperID            uint64
	MessageID          uint64
	ReadType           string
	Content            string
	CostUSD            float64
	GuestReservationID *string
}

type ProjectReadResult struct {
	ID                   uint64    `json:"-"`
	ProjectPaperID       uint64    `json:"-"`
	MessageID            uint64    `json:"-"`
	SourceConversationID uint64    `json:"-"`
	ReadType             string    `json:"readType"`
	SourceConversationNo uint32    `json:"sourceConversationNo"`
	CreatedAt            time.Time `json:"createdAt"`
	Content              string    `json:"content"`
}

// ResearchSelection is the authoritative owner-scoped read model used by GET /api/session.
type ResearchSelection struct {
	ActiveProjectNo      *uint32 `json:"activeProjectNo"`
	ActiveConversationNo *uint32 `json:"activeConversationNo"`
}
