package controller

import (
	"errors"
	"net/http"
	"strings"

	"paperassistant/internal/dao"
	"paperassistant/internal/provider/fulltext"
	"paperassistant/internal/service"
)

// Papers exposes account-private full text (ADR 0019). Routes are mounted behind
// RequireAccount; the account is always derived from the verified token, never the request.
type Papers struct {
	app         *service.Application
	platform    *Platform
	maxPDFBytes int64
}

func NewPapers(app *service.Application, platform *Platform, maxPDFBytes int64) *Papers {
	return &Papers{app: app, platform: platform, maxPDFBytes: maxPDFBytes}
}

func (p *Papers) paperRequest(w http.ResponseWriter, r *http.Request, mutation bool) (service.WebActor, uint64, bool) {
	if mutation && (p.platform == nil || !p.platform.sameOrigin(r)) {
		writeStatusError(w, http.StatusForbidden, "ORIGIN_MISMATCH", "请求来源无效")
		return service.WebActor{}, 0, false
	}
	actor, ok := webActorFromContext(r)
	if !ok || actor.Type != service.WebActorAccount {
		writeStatusError(w, http.StatusForbidden, "ACCOUNT_REQUIRED", "上传私有全文需要登录账号")
		return service.WebActor{}, 0, false
	}
	paperID, err := pathUint64(r, "paperId")
	if err != nil || paperID == 0 {
		writeStatusError(w, http.StatusBadRequest, "INVALID_REQUEST", "请求参数无效")
		return service.WebActor{}, 0, false
	}
	return actor, paperID, true
}

// UploadPrivateFullText accepts the raw PDF bytes as the request body (Content-Type
// application/pdf). The PDF itself is discarded after text extraction.
func (p *Papers) UploadPrivateFullText(w http.ResponseWriter, r *http.Request) {
	actor, paperID, ok := p.paperRequest(w, r, true)
	if !ok {
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if contentType != "application/pdf" && contentType != "application/octet-stream" {
		writeStatusError(w, http.StatusUnsupportedMediaType, "NOT_PDF", "请上传 PDF 文件")
		return
	}
	if r.ContentLength > p.maxPDFBytes {
		writeStatusError(w, http.StatusRequestEntityTooLarge, "PDF_TOO_LARGE", "PDF 文件过大")
		return
	}
	body := http.MaxBytesReader(w, r.Body, p.maxPDFBytes+1)
	status, err := p.app.UploadPrivateFullText(r.Context(), actor, paperID, body)
	if err != nil {
		writePrivateFullTextError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fullText": status})
}

func (p *Papers) PrivateFullTextStatus(w http.ResponseWriter, r *http.Request) {
	actor, paperID, ok := p.paperRequest(w, r, false)
	if !ok {
		return
	}
	status, err := p.app.PrivateFullTextStatus(r.Context(), actor, paperID)
	if err != nil {
		writePrivateFullTextError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fullText": status})
}

func (p *Papers) DeletePrivateFullText(w http.ResponseWriter, r *http.Request) {
	actor, paperID, ok := p.paperRequest(w, r, true)
	if !ok {
		return
	}
	if err := p.app.DeletePrivateFullText(r.Context(), actor, paperID); err != nil {
		writePrivateFullTextError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writePrivateFullTextError(w http.ResponseWriter, err error) {
	var maxBytes *http.MaxBytesError
	switch {
	case errors.Is(err, service.ErrAccountRequired):
		writeStatusError(w, http.StatusForbidden, "ACCOUNT_REQUIRED", "上传私有全文需要登录账号")
	case errors.Is(err, service.ErrValidation):
		writeStatusError(w, http.StatusBadRequest, "INVALID_REQUEST", "请求参数无效")
	case errors.Is(err, service.ErrUploadRateLimited):
		writeStatusError(w, http.StatusTooManyRequests, "RATE_LIMITED", "上传过于频繁，请稍后再试")
	case errors.Is(err, dao.ErrPaperNotFound):
		writeStatusError(w, http.StatusNotFound, "PAPER_NOT_FOUND", "论文不存在")
	case errors.Is(err, dao.ErrNotFound):
		writeStatusError(w, http.StatusNotFound, "FULLTEXT_NOT_FOUND", "这篇论文没有你的私有全文")
	case errors.Is(err, fulltext.ErrPDFTooLarge), errors.As(err, &maxBytes):
		writeStatusError(w, http.StatusRequestEntityTooLarge, "PDF_TOO_LARGE", "PDF 文件过大")
	case errors.Is(err, fulltext.ErrNotPDF):
		writeStatusError(w, http.StatusUnsupportedMediaType, "NOT_PDF", "上传的文件不是有效的 PDF")
	case errors.Is(err, fulltext.ErrUnreadable):
		writeStatusError(w, http.StatusUnprocessableEntity, "FULLTEXT_INVALID", "无法从这个 PDF 中提取可校验的论文正文")
	case errors.Is(err, service.ErrFullTextMismatch):
		writeStatusError(w, http.StatusUnprocessableEntity, "FULLTEXT_MISMATCH", "上传的 PDF 与所选论文不匹配（未在开头找到该论文的 DOI 或标题）")
	default:
		w.Header().Set("Retry-After", "30")
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "全文服务暂时不可用")
	}
}
