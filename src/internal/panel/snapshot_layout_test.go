package panel

import (
	"regexp"
	"strings"
	"testing"

	"vpsmgr/internal/db"
)

func TestSnapshotScheduleCompactControls(t *testing.T) {
	srv, _ := newTestServer(t)
	rowPattern := regexp.MustCompile(`(?s)<div class="snap-schedule-controls">(.*?)</div>`)
	formPattern := regexp.MustCompile(`(?s)<form\b[^>]*>.*?</form>`)
	for _, lang := range []string{"zh", "en"} {
		for _, state := range []string{"enabled", "disabled", "unavailable"} {
			t.Run(lang+"/"+state, func(t *testing.T) {
				data := pageData{
					User: &db.User{Name: "alice", Status: db.StatusReady}, Lang: lang,
					Prefix: srv.prefix(), SnapshotLimit: 3,
					SnapshotSchedule: &db.SnapshotSchedule{Enabled: state == "enabled", IntervalHours: 7},
				}
				if state == "unavailable" {
					data.SnapshotScheduleError = "schedule unavailable"
				}
				data.prepareSnapshotUI()
				body := srv.renderToString(t, "overview.html", data)
				hint, interval, save, disable := "Enabling needs a free slot; old automatic checkpoints are deleted automatically.", "Every (h)", "Save", "Disable"
				if lang == "zh" {
					hint, interval, save, disable = "启用需至少一个空位，旧的自动检查点会自动删除。", "每隔(h)", "保存间隔", "关闭自动服务"
				}
				if !strings.Contains(body, `<p class="muted">`+hint+`</p>`) {
					t.Error("missing concise automatic checkpoint hint")
				}
				for _, removed := range []string{"并为自动检查点保留一个名额", "期间可能临时超出总额度一个", "reserves one position", "usage may briefly exceed the total by one"} {
					if strings.Contains(body, removed) {
						t.Errorf("verbose schedule hint remains: %q", removed)
					}
				}
				row := rowPattern.FindStringSubmatch(body)
				if row == nil {
					t.Fatal("schedule controls must share one horizontal wrapper")
				}
				forms := formPattern.FindAllString(row[1], -1)
				wantForms := 2
				if state == "disabled" {
					wantForms = 1
				}
				if len(forms) != wantForms || strings.Count(row[1], "<form ") != wantForms {
					t.Fatalf("schedule row has %d forms, want %d separate, non-nested POST forms", len(forms), wantForms)
				}
				for _, form := range forms {
					if !strings.Contains(form, `action="`+srv.prefix()+`/snapshot-schedule" method="post"`) {
						t.Error("schedule form lost its POST endpoint")
					}
				}
				for _, want := range []string{
					`id="snapScheduleForm"`, `name="enabled" value="1"`,
					`<label for="snapHours">` + interval + `</label>`,
					`id="snapHours" name="hours" min="1"`, ` step="1" required value="7"`,
					`type="submit" id="snapScheduleSave"`,
				} {
					if !strings.Contains(forms[0], want) {
						t.Errorf("interval form missing %q", want)
					}
				}
				if state == "enabled" && !strings.Contains(forms[0], `>`+save+`</button>`) {
					t.Error("enabled interval save control must keep its compact label")
				}
				if wantForms == 2 {
					for _, want := range []string{`id="snapScheduleDisableForm"`, `name="enabled" value="0"`, `type="submit" id="snapScheduleDisable"`, `>` + disable + `</button>`} {
						if !strings.Contains(forms[1], want) {
							t.Errorf("separate disable form missing %q", want)
						}
					}
					if strings.Contains(forms[1], `name="hours"`) {
						t.Error("disabling must not depend on interval input validation")
					}
				}
			})
		}
	}
}

func TestSnapshotScheduleNoWrapPhoneCSS(t *testing.T) {
	srv, _ := newTestServer(t)
	body := srv.renderToString(t, "overview.html", pageData{User: &db.User{Name: "alice"}})
	// Both levels must opt out of .btnrow wrapping. The shrinkable interval
	// absorbs spare width while the label and the independent buttons stay whole.
	for _, rule := range []struct {
		selector string
		want     []string
	}{
		{".snap-schedule-controls", []string{"display:flex", "align-items:center", "flex-wrap:nowrap", "min-width:0"}},
		{"#snapScheduleForm", []string{"display:flex", "align-items:center", "flex-wrap:nowrap", "flex:1", "min-width:0", "margin:0"}},
		{"#snapScheduleDisableForm", []string{"flex:none", "margin:0"}},
		{".snap-schedule-controls label,.snap-schedule-controls .btn", []string{"white-space:nowrap", "flex:none"}},
		{"#snapHours", []string{"min-width:3rem", "max-width:8rem", "flex:1 1 4.5rem"}},
	} {
		match := regexp.MustCompile(regexp.QuoteMeta(rule.selector) + `\s*\{([^}]*)\}`).FindStringSubmatch(body)
		if match == nil {
			t.Fatalf("missing schedule layout rule %q", rule.selector)
		}
		declarations := ";" + strings.TrimSpace(match[1]) + ";"
		for _, want := range rule.want {
			if !strings.Contains(declarations, ";"+want+";") {
				t.Errorf("%s missing %q", rule.selector, want)
			}
		}
		if strings.Contains(match[1], "overflow") {
			t.Errorf("%s must fit rather than hide or horizontally scroll controls", rule.selector)
		}
	}
	// At 320px, overlay/modal borders and padding leave 258px for controls.
	// Compact spacing/type and button padding keep the Chinese row inside it.
	phone := regexp.MustCompile(`(?s)@media\s*\(max-width:420px\)\s*\{(.*?)\n\}`).FindStringSubmatch(body)
	if phone == nil {
		t.Fatal("missing compact phone schedule styles")
	}
	for _, want := range []string{
		`#snapModal{padding:.5rem}`,
		`#snapModal .modal{padding-inline:.75rem}`,
		`#snapModal .snap-schedule{padding:.5rem}`,
		`.snap-schedule-controls,#snapScheduleForm{gap:.25rem}`,
		`.snap-schedule-controls label,.snap-schedule-controls .btn,#snapHours{font-size:.75rem}`,
		`.snap-schedule-controls .btn{padding:.35rem .3rem}`,
		`#snapHours{padding:.35rem .2rem}`,
	} {
		if !strings.Contains(phone[1], want) {
			t.Errorf("phone layout missing %q", want)
		}
	}
}
