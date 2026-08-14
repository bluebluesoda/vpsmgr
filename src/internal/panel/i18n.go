package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Panel UI languages. "en" is the default when the browser does not ask for
// Chinese — webui renders zh or en based on the browser, with English as
// fallback.
const (
	langZh = "zh"
	langEn = "en"
)

const langCookie = "vpsmgr_lang"

// langCtxKey carries the resolved language in the request context, computed once
// in the top-level handler so every route below sees the same value.
const langCtxKey ctxKey = 100

// normalLang maps a raw tag to a supported code, or "" if unsupported.
func normalLang(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch {
	case strings.HasPrefix(v, "zh"):
		return langZh
	case strings.HasPrefix(v, "en"):
		return langEn
	}
	return ""
}

// langFromRequirement resolves the request language: ?lang= param wins,
// then the vpsmgr_lang cookie, then the browser Accept-Language header
// (first tag that resolves to zh or en). Defaults to English.
func (s *Server) lang(r *http.Request) string {
	if l := normalLang(r.URL.Query().Get("lang")); l != "" {
		return l
	}
	if c, err := r.Cookie(langCookie); err == nil {
		if l := normalLang(c.Value); l != "" {
			return l
		}
	}
	return langFromHeader(r.Header.Get("Accept-Language"))
}

// langFromHeader picks a supported language from the first "language" tag in
// order in the Accept-Language header. English when nothing resolves.
func langFromHeader(h string) string {
	for _, field := range strings.Split(h, ",") {
		tag := strings.TrimSpace(strings.SplitN(field, ";", 2)[0])
		if tag == "" {
			continue
		}
		if l := normalLang(tag); l != "" {
			return l
		}
	}
	return langEn
}

// langContext carries the resolved language through the request context.
func withLang(ctx context.Context, l string) context.Context {
	return context.WithValue(ctx, langCtxKey, l)
}

// t formats a catalog text into the request's language. The catalog keys are
// stable identifiers; args are %-formatted into the chosen language's template.
func (s *Server) t(r *http.Request, key string, args ...any) string {
	l, _ := r.Context().Value(langCtxKey).(string)
	return tr(l, key, args...)
}

// tr formats a catalog text into the given language. The catalog carries only
// server-side strings (flash banners, login errors) that are produced in Go;
// everything rendered by the templates lives in the templates themselves, one
// zh and one en variant toggled by {{if eq .Lang "zh"}}.
func tr(l, key string, args ...any) string {
	m := map[string][2]string{ // key -> [zh, en]
		"err_bad_login":     {"用户名或密码错误", "invalid credentials"},
		"err_too_many":      {"尝试过于频繁，请 1 分钟后再试", "Too many attempts, please wait 1 minute"},
		"err_pass_mismatch": {"两次输入的密码不一致", "The two passwords do not match"},
		"new_root_password": {"新的 root 密码：\n%[1]v", "New root password:\n%[1]v"},
		"reinstall_done":    {"重装完成，新的 root 密码：\n%[1]v", "Reinstall complete. Root password:\n%[1]v"},
	}
	pair, ok := m[key]
	if !ok {
		return key
	}
	if l == langZh {
		return fmt.Sprintf(pair[0], args...)
	}
	return fmt.Sprintf(pair[1], args...)
}