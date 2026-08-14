package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Admin UI languages, mirroring the user panel: "en" is the default when the
// browser does not ask for Chinese, and the choice persists in a cookie.
const (
	langZh = "zh"
	langEn = "en"
)

const langCookie = "vpsmgr_admin_lang"

// langCtxKey carries the resolved language in the request context, computed
// once in the top-level handler so every route below sees the same value.
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

// lang resolves the request language: ?lang= param wins, then the
// vpsmgr_admin_lang cookie, then the browser Accept-Language header (first tag
// that resolves to zh or en). Defaults to English — same as the user panel.
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

// t formats a catalog text into the request's language. Server-side strings
// (flash banners, login errors) live here; everything rendered by the
// templates uses {{if eq .Lang "zh"}} inline, same as the user panel.
func (s *Server) t(r *http.Request, key string, args ...any) string {
	l, _ := r.Context().Value(langCtxKey).(string)
	return tr(l, key, args...)
}

// tr formats a catalog text into the given language. The catalog keys are
// stable identifiers; args are %-formatted into the chosen language's template.
func tr(l, key string, args ...any) string {
	m := map[string][2]string{ // key -> [zh, en]
		"err_bad_login":      {"管理员密码错误", "invalid admin password"},
		"err_too_many":       {"尝试过于频繁，请 1 分钟后再试", "Too many attempts, please wait 1 minute"},
		"err_not_configured": {"管理员尚未初始化 — 请运行 `vps admin-passwd`", "admin not configured yet — run `vps admin-passwd`"},
		"err_pass_mismatch":  {"两次输入的密码不一致", "The two passwords do not match"},
		"err_pass_short":     {"管理员密码至少需要 14 位", "admin password must be at least 14 characters"},
		"user_created":       {"用户已创建：\n%[1]v", "user created:\n%[1]v"},
		"user_deleted":       {"用户 %[1]v 已删除", "user %[1]v deleted"},
		"quota_updated":      {"已更新 %[1]v 的配额", "quotas updated for %[1]v"},
		"power_ok":           {"%[1]v %[2]v 成功", "%[2]v %[1]v ok"},
		"new_panel_password": {"用户 %[1]v 的面板密码已重置：\n%[2]v\n面板：%[3]v", "%[1]v panel password reset:\n%[2]v\npanel: %[3]v"},
		"admin_pass_changed": {"管理员密码已修改", "admin password changed"},
		"err_invalid_disk":   {"磁盘必须是整数 GiB", "disk must be an integer (GiB)"},
		"domain_deleted":     {"域名 %[1]v 已删除", "domain %[1]v deleted"},
		"domains_updated":    {"域名设置已保存", "domain settings saved"},
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
