package admin

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestKnowledgeAdminCRUD(t *testing.T) {
	srv, d := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	// Page renders (empty state).
	rr := doReq(t, h, http.MethodGet, prefix+"/knowledge", nil, cookie)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "New article") {
		t.Fatalf("GET /knowledge = %d, body missing New article", rr.Code)
	}

	// Preview renders Markdown to HTML as JSON (the encoder escapes "<" as
	// \u003c, which JSON-decodes back to markup in the browser).
	rr = doReq(t, h, http.MethodPost, prefix+"/knowledge-preview",
		url.Values{"content": {"# Hi\n\n`x`"}}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview = %d", rr.Code)
	}
	var pv struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &pv); err != nil {
		t.Fatalf("preview JSON: %v (%s)", err, rr.Body.String())
	}
	if !strings.Contains(pv.HTML, "<h1>Hi</h1>") || !strings.Contains(pv.HTML, "<code>x</code>") {
		t.Fatalf("preview html = %q", pv.HTML)
	}

	// The browser posts the editor body as multipart/form-data (a fetch with a
	// FormData body). ParseForm alone ignores multipart bodies, which silently
	// rendered the preview empty — assert the multipart path works.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("content", "# Multi\n\n`y`"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, prefix+"/knowledge-preview", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	mrr := httptest.NewRecorder()
	h.ServeHTTP(mrr, req)
	if mrr.Code != http.StatusOK {
		t.Fatalf("multipart preview = %d (%s)", mrr.Code, mrr.Body.String())
	}
	pv.HTML = ""
	if err := json.Unmarshal(mrr.Body.Bytes(), &pv); err != nil {
		t.Fatalf("multipart preview JSON: %v", err)
	}
	if !strings.Contains(pv.HTML, "<h1>Multi</h1>") || !strings.Contains(pv.HTML, "<code>y</code>") {
		t.Fatalf("multipart preview html = %q", pv.HTML)
	}

	// An empty title is refused.
	rr = doReq(t, h, http.MethodPost, prefix+"/knowledge-save",
		url.Values{"title": {"  "}, "content": {"x"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("empty-title save = %d, want 302", rr.Code)
	}
	if list, _ := d.ListKnowledge(); len(list) != 0 {
		t.Fatalf("article created without a title: %+v", list)
	}

	// Create.
	rr = doReq(t, h, http.MethodPost, prefix+"/knowledge-save",
		url.Values{"title": {"Getting started"}, "content": {"**bold** text"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("save = %d", rr.Code)
	}
	list, err := d.ListKnowledge()
	if err != nil || len(list) != 1 || list[0].Title != "Getting started" {
		t.Fatalf("list = %+v, err=%v", list, err)
	}
	id := strconv.FormatInt(list[0].ID, 10)

	// The edit page prefills the editor.
	rr = doReq(t, h, http.MethodGet, prefix+"/knowledge?edit="+id, nil, cookie)
	if body := rr.Body.String(); !strings.Contains(body, "&gt;**bold** text") && !strings.Contains(body, "**bold** text") {
		t.Fatalf("edit page missing the content: %s", body)
	}

	// Update.
	rr = doReq(t, h, http.MethodPost, prefix+"/knowledge-save",
		url.Values{"id": {id}, "title": {"Getting started v2"}, "content": {"updated"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("update = %d", rr.Code)
	}
	if k, _ := d.GetKnowledge(list[0].ID); k == nil || k.Title != "Getting started v2" || k.Content != "updated" {
		t.Fatalf("update did not stick: %+v", k)
	}

	// Delete.
	rr = doReq(t, h, http.MethodPost, prefix+"/knowledge-del", url.Values{"id": {id}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("delete = %d", rr.Code)
	}
	if list, _ := d.ListKnowledge(); len(list) != 0 {
		t.Fatalf("article survived delete: %+v", list)
	}
}

func TestKnowledgeRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	rr := doReq(t, h, http.MethodGet, prefix+"/knowledge", nil, nil)
	if rr.Code == http.StatusOK {
		t.Fatal("unauthenticated GET /knowledge returned 200")
	}
}
