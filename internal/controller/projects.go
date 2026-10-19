package controller

import (
	"errors"
	"net/http"
	"strconv"

	"paperassistant/internal/dao"
	"paperassistant/internal/service"
)

// Projects exposes deterministic Web project-tree mutations. Actor ownership is always derived
// from auth context; database IDs are never accepted as route parameters.
type Projects struct {
	app      *service.Application
	platform *Platform
}

func NewProjects(app *service.Application, platform *Platform) *Projects {
	return &Projects{app: app, platform: platform}
}
func (p *Projects) actor(w http.ResponseWriter, r *http.Request) (service.WebActor, bool) {
	actor, ok := webActorFromContext(r)
	if !ok {
		writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "需要有效的账号或访客会话")
	}
	return actor, ok
}
func (p *Projects) mutation(w http.ResponseWriter, r *http.Request) bool {
	if p.platform == nil || !p.platform.sameOrigin(r) {
		writeStatusError(w, http.StatusForbidden, "ORIGIN_MISMATCH", "请求来源无效")
		return false
	}
	return true
}
func queryProjectLimit(r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 20
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return -1
	}
	return value
}
func projectNo(r *http.Request) (uint32, error) {
	value, err := strconv.ParseUint(r.PathValue("projectNo"), 10, 32)
	return uint32(value), err
}
func conversationNo(r *http.Request) (uint32, error) {
	value, err := strconv.ParseUint(r.PathValue("conversationNo"), 10, 32)
	return uint32(value), err
}

func (p *Projects) EnsureActive(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	project, conversation, created, err := p.app.EnsureActiveProject(r.Context(), actor)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "project": project, "conversation": conversation})
}
func (p *Projects) List(w http.ResponseWriter, r *http.Request) {
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	page, err := p.app.ListProjects(r.Context(), actor, r.URL.Query().Get("status"), queryProjectLimit(r), r.URL.Query().Get("cursor"))
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (p *Projects) Create(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	var payload struct {
		Title string `json:"title"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	project, conversation, err := p.app.CreateProject(r.Context(), actor, payload.Title)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"project": project, "conversation": conversation})
}
func (p *Projects) Rename(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	no, err := projectNo(r)
	if err != nil || no == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	var payload struct {
		Title string `json:"title"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	project, err := p.app.RenameProject(r.Context(), actor, no, payload.Title)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": project})
}
func (p *Projects) Select(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	no, err := projectNo(r)
	if err != nil || no == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	project, conversation, err := p.app.SelectProject(r.Context(), actor, no)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "conversation": conversation})
}
func (p *Projects) Archive(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	no, err := projectNo(r)
	if err != nil || no == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	archived, active, conversation, err := p.app.ArchiveProject(r.Context(), actor, no)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"archivedProject": archived, "activeProject": active, "activeConversation": conversation})
}
func (p *Projects) Restore(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	no, err := projectNo(r)
	if err != nil || no == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	project, err := p.app.RestoreProject(r.Context(), actor, no)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": project})
}

func (p *Projects) ListConversations(w http.ResponseWriter, r *http.Request) {
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	pn, err := projectNo(r)
	if err != nil || pn == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	page, err := p.app.ListProjectConversations(r.Context(), actor, pn, r.URL.Query().Get("status"), queryProjectLimit(r), r.URL.Query().Get("cursor"))
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (p *Projects) CreateConversation(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	pn, err := projectNo(r)
	if err != nil || pn == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	var payload struct {
		Title string `json:"title"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	conversation, err := p.app.CreateProjectConversation(r.Context(), actor, pn, payload.Title)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"conversation": conversation})
}
func (p *Projects) RenameConversation(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	pn, e1 := projectNo(r)
	cn, e2 := conversationNo(r)
	if e1 != nil || e2 != nil || pn == 0 || cn == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	var payload struct {
		Title string `json:"title"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	conversation, err := p.app.RenameProjectConversation(r.Context(), actor, pn, cn, payload.Title)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation": conversation})
}
func (p *Projects) SelectConversation(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	pn, e1 := projectNo(r)
	cn, e2 := conversationNo(r)
	if e1 != nil || e2 != nil || pn == 0 || cn == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	project, conversation, err := p.app.SelectProjectConversation(r.Context(), actor, pn, cn)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "conversation": conversation})
}
func (p *Projects) ArchiveConversation(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	pn, e1 := projectNo(r)
	cn, e2 := conversationNo(r)
	if e1 != nil || e2 != nil || pn == 0 || cn == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	archived, active, err := p.app.ArchiveProjectConversation(r.Context(), actor, pn, cn)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"archivedConversation": archived, "activeConversation": active})
}
func (p *Projects) RestoreConversation(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	pn, e1 := projectNo(r)
	cn, e2 := conversationNo(r)
	if e1 != nil || e2 != nil || pn == 0 || cn == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	conversation, err := p.app.RestoreProjectConversation(r.Context(), actor, pn, cn)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation": conversation})
}

func (p *Projects) ListPapers(w http.ResponseWriter, r *http.Request) {
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	pn, err := projectNo(r)
	if err != nil || pn == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	page, err := p.app.ListProjectPapers(r.Context(), actor, pn, r.URL.Query().Get("status"), queryProjectLimit(r), r.URL.Query().Get("cursor"))
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (p *Projects) AddPaper(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	pn, err := projectNo(r)
	paperID, e2 := pathUint64(r, "paperId")
	if err != nil || e2 != nil || pn == 0 || paperID == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	paper, created, err := p.app.AddProjectPaper(r.Context(), actor, pn, paperID)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"paper": paper, "created": created})
}
func (p *Projects) RemovePaper(w http.ResponseWriter, r *http.Request) {
	if !p.mutation(w, r) {
		return
	}
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	pn, err := projectNo(r)
	paperID, e2 := pathUint64(r, "paperId")
	if err != nil || e2 != nil || pn == 0 || paperID == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	if err = p.app.RemoveProjectPaper(r.Context(), actor, pn, paperID); err != nil {
		writeProjectError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (p *Projects) ReadResults(w http.ResponseWriter, r *http.Request) {
	actor, ok := p.actor(w, r)
	if !ok {
		return
	}
	pn, err := projectNo(r)
	paperID, e2 := pathUint64(r, "paperId")
	if err != nil || e2 != nil || pn == 0 || paperID == 0 {
		writeProjectError(w, service.ErrValidation)
		return
	}
	page, err := p.app.ListProjectReadResults(r.Context(), actor, pn, paperID, queryProjectLimit(r), r.URL.Query().Get("cursor"))
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func writeProjectError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrValidation):
		writeStatusError(w, http.StatusBadRequest, "INVALID_REQUEST", "请求参数无效")
	case errors.Is(err, service.ErrInvalidCursor):
		writeStatusError(w, http.StatusBadRequest, "INVALID_CURSOR", "分页游标无效")
	case errors.Is(err, dao.ErrProjectArchived):
		writeStatusError(w, http.StatusConflict, "PROJECT_ARCHIVED", "项目已归档")
	case errors.Is(err, dao.ErrConversationArchived):
		writeStatusError(w, http.StatusConflict, "CONVERSATION_ARCHIVED", "会话已归档")
	case errors.Is(err, dao.ErrProjectPaperNotFound):
		writeStatusError(w, http.StatusNotFound, "PROJECT_PAPER_NOT_FOUND", "项目论文不存在")
	case errors.Is(err, dao.ErrResourceStateConflict):
		writeStatusError(w, http.StatusConflict, "RESOURCE_STATE_CONFLICT", "资源状态已变化")
	case errors.Is(err, dao.ErrGuestUnavailable):
		writeStatusError(w, http.StatusUnauthorized, "INVALID_GUEST_SESSION", "访客会话无效")
	case errors.Is(err, dao.ErrProjectNotFound):
		writeStatusError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
	case errors.Is(err, dao.ErrConversationNotFound):
		writeStatusError(w, http.StatusNotFound, "CONVERSATION_NOT_FOUND", "会话不存在")
	case errors.Is(err, dao.ErrPaperNotFound):
		writeStatusError(w, http.StatusNotFound, "PAPER_NOT_FOUND", "论文不存在")
	case errors.Is(err, dao.ErrNotFound):
		writeStatusError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
	default:
		w.Header().Set("Retry-After", "30")
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "项目服务暂时不可用")
	}
}
