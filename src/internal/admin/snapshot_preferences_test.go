package admin

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestSnapshotQuotaBrowserPreferences(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("requires Node.js to execute the rendered browser preference script")
	}
	srv, _ := newTestServer(t)
	body := srv.renderToString(t, "admin_overview.html", pageData{Lang: langEn, ShareEnabled: true})
	start := strings.Index(body, "var KEY = 'vpsmgr_admin_create';")
	if start < 0 {
		t.Fatal("create-user preference script not found")
	}
	end := strings.Index(body[start:], "})();")
	if end < 0 {
		t.Fatal("create-user preference script is incomplete")
	}
	payload, err := json.Marshal("(function(){\n" + body[start:start+end+len("})();")])
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "-e", `
const assert = require('node:assert/strict');
const script = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const key = 'vpsmgr_admin_create';
function mount(storage = new Map(), blocked = false, batch = true) {
  function field(initial) {
    let value = String(initial);
    const listeners = {};
    return {
      get value() { return value; }, set value(v) { value = String(v); },
      classList: {add() {}},
      addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
      emit(type) { for (const fn of listeners[type] || []) fn({target: this}); }
    };
  }
  const fields = {};
  for (const [id, value] of Object.entries({cfCpu: 1, cfMem: 1024, cfDisk: 10,
      cfBandwidth: 0, cfDays: 0, cfSnapshots: 1, cfBtn: '', createForm: '', quotaSnapshots: 11})) {
    fields[id] = field(value);
  }
  if (batch) { fields.batchSnapshots = field(1); fields.batchForm = field(''); }
  const localStorage = {
    getItem(k) { if (blocked) throw Error('storage blocked'); return storage.get(k) ?? null; },
    setItem(k, v) { if (blocked) throw Error('storage blocked'); storage.set(k, v); }
  };
  new Function('document', 'localStorage', 'syncCpuStep', 'CREATE_ING', script)(
    {getElementById(id) { return fields[id] || null; }}, localStorage, () => {}, 'Creating');
  return fields;
}
let storage = new Map();
let fields = mount(storage);
assert.equal(fields.cfSnapshots.value, '1');
assert.equal(fields.batchSnapshots.value, '1');
fields.cfSnapshots.value = '0'; fields.cfSnapshots.emit('input');
assert.equal(JSON.parse(storage.get(key)).snapshots, '0');
assert.equal(fields.batchSnapshots.value, '0');
fields = mount(storage);
assert.equal(fields.cfSnapshots.value, '0', 'zero must survive reload');
assert.equal(fields.batchSnapshots.value, '0');
fields.batchSnapshots.value = '8'; fields.batchSnapshots.emit('input');
assert.equal(fields.cfSnapshots.value, '8');
assert.equal(JSON.parse(storage.get(key)).snapshots, '8');
fields = mount(storage);
assert.equal(fields.cfSnapshots.value, '8');
assert.equal(fields.batchSnapshots.value, '8');
fields.batchSnapshots.value = '7'; fields.batchForm.emit('submit');
assert.equal(JSON.parse(storage.get(key)).snapshots, '7', 'batch submission remembers autofill');
fields.cfSnapshots.value = '3'; fields.createForm.emit('submit');
assert.equal(JSON.parse(storage.get(key)).snapshots, '3', 'single submission remembers autofill');
assert.equal(fields.quotaSnapshots.value, '11', 'create preferences must not change an existing user quota');
const before = storage.get(key);
fields.quotaSnapshots.value = '12'; fields.quotaSnapshots.emit('input');
assert.equal(storage.get(key), before, 'editing an existing user must not overwrite creation preferences');

storage = new Map([[key, JSON.stringify({cpu: '2', mem: '512', disk: '4', bandwidth: '0', days: '7'})]]);
fields = mount(storage);
assert.equal(fields.cfSnapshots.value, '1', 'old preferences without snapshots use the built-in default');
assert.equal(fields.cfCpu.value, '2');
assert.equal(fields.cfMem.value, '512');
fields.cfSnapshots.value = '5'; fields.cfSnapshots.emit('input');
assert.deepEqual(JSON.parse(storage.get(key)), {cpu: '2', mem: '512', disk: '4', bandwidth: '0', snapshots: '5', days: '7'});
for (const value of [0, '0', 5, '5']) {
  fields = mount(new Map([[key, JSON.stringify({snapshots: value})]]));
  assert.equal(fields.cfSnapshots.value, String(value));
}
for (const value of [null, false, -1, '1.5', '', {}, ['5'], 'invalid']) {
  fields = mount(new Map([[key, JSON.stringify({snapshots: value})]]));
  assert.equal(fields.cfSnapshots.value, '1', 'invalid saved quota must not replace the default');
}
fields = mount(new Map([[key, 'not JSON']]));
assert.equal(fields.cfSnapshots.value, '1');
fields = mount(new Map(), true);
assert.equal(fields.cfSnapshots.value, '1');
fields.cfSnapshots.value = '0'; fields.cfSnapshots.emit('input');
assert.equal(fields.batchSnapshots.value, '0', 'blocked storage must not break editing');
fields = mount(new Map([[key, JSON.stringify({snapshots: '0'})]]), false, false);
assert.equal(fields.cfSnapshots.value, '0', 'single creation works without a batch form');
fields.cfSnapshots.value = '4'; fields.cfSnapshots.emit('input');
console.log('PASS: quota save/reload, zero, shared batch preference, old/corrupt/blocked storage and existing-user isolation');
`)
	cmd.Stdin = strings.NewReader(string(payload))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser preference script: %v\n%s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)))
}
