package service

import (
	"context"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

type Admin struct {
	store     dao.Store
	emergency *EmergencyStop
}

func NewAdmin(store dao.Store, emergency *EmergencyStop) *Admin {
	return &Admin{store: store, emergency: emergency}
}

func (a *Admin) Health(ctx context.Context) error { return a.store.Ping(ctx) }
func (a *Admin) Overview(ctx context.Context, days int) (domain.Overview, error) {
	return a.store.Overview(ctx, days)
}
func (a *Admin) Users(ctx context.Context, limit int) ([]domain.AdminUser, error) {
	return a.store.ListAdminUsers(ctx, limit)
}
func (a *Admin) Conversations(ctx context.Context, userID uint64) ([]domain.Conversation, error) {
	return a.store.ListAdminConversations(ctx, userID, 100)
}
func (a *Admin) Messages(ctx context.Context, userID, conversationID uint64, limit int) ([]domain.Message, error) {
	return a.store.ListConversationMessages(ctx, userID, conversationID, limit)
}
func (a *Admin) RestoreConversation(ctx context.Context, userID, conversationID uint64) error {
	return a.store.RestoreConversation(ctx, userID, conversationID)
}

func (a *Admin) EmergencyStopState() *EmergencyStopState { return a.emergency.State() }
func (a *Admin) EngageEmergencyStop(reason string) EmergencyStopState {
	return a.emergency.Engage(reason)
}
func (a *Admin) ReleaseEmergencyStop() { a.emergency.Release() }
