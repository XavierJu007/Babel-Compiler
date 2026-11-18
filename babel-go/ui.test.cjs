const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
// Parse the whole shipped script, then run application functions without the
// decorative canvas animation, startup network requests, or polling timers.
new vm.Script(script, { filename: 'index.html' });
const app = 'let isOverdrive = false;\n' + script.slice(
    script.indexOf('// ==================== 2.'), script.lastIndexOf('    setInterval('));

class Element {
    constructor() {
        this.className = '';
        this.children = [];
        this.style = {};
        this.dataset = {};
        this.value = '';
        this.checked = false;
        this.disabled = false;
        this._text = '';
        this.classList = {
            add: (...names) => this.setClasses([...this.classes(), ...names]),
            remove: (...names) => this.setClasses(this.classes().filter(name => !names.includes(name))),
            contains: name => this.classes().includes(name),
            toggle: (name, force) => {
                const add = force === undefined ? !this.classes().includes(name) : force;
                add ? this.classList.add(name) : this.classList.remove(name);
                return add;
            }
        };
    }
    classes() { return this.className.split(/\s+/).filter(Boolean); }
    setClasses(names) { this.className = [...new Set(names)].join(' '); }
    set textContent(value) { this._text = String(value); this.children = []; }
    get textContent() { return this._text + this.children.map(child => child.textContent).join(''); }
    set innerHTML(value) { this._text = String(value); this.children = []; }
    appendChild(child) { this.children.push(child); return child; }
    replaceChildren(...children) { this._text = ''; this.children = children; }
}

function setup(responses = []) {
    const elements = new Map();
    const get = id => {
        if (!elements.has(id)) elements.set(id, new Element());
        return elements.get(id);
    };
    const requests = [];
    const context = vm.createContext({
        document: { getElementById: get, createElement: () => new Element() },
        window: { location: { origin: 'http://127.0.0.1:8080' } },
        fetch: async (url, options) => {
            requests.push({ url, options });
            let response = responses.shift();
            if (typeof response === 'function') response = await response();
            if (response instanceof Error) throw response;
            if (!response) throw new Error('Unexpected fetch');
            const status = response.status || 200;
            return { status, ok: status >= 200 && status < 300,
                text: async () => response.raw ?? JSON.stringify(response.data) };
        },
        setTimeout: () => {},
        navigator: { clipboard: { writeText: async () => {} } },
        alert: message => { throw new Error(`Unexpected alert: ${message}`); }
    });
    vm.runInContext(app, context);
    get('rawInput').value = '禁止删除订单；金额 > 50 元时审批。';
    get('modeSelect').value = 'standard';
    for (const id of ['statsBadge', 'cacheBadge', 'auditBox', 'invariantCapsules', 'logsError']) {
        get(id).className = 'hidden';
    }
    return { context, get, requests };
}

function validation(status, method = 'llm_compare', issues = []) {
    return { status, method, issues, policy_version: 'semantic-gate-v2' };
}

function result(overrides = {}) {
    return {
        compiled: '禁止删除订单；金额 > 50 元时审批。',
        validation: validation('passed'), cache_hit: true,
        stats: { original_tokens: 100, compiled_tokens: 70, measurement_kind: 'estimated',
            measurement_source: 'heuristic-v2', measurement_scope: 'text',
            compression_ratio: '实测节省 99%' }, ...overrides
    };
}

test('legacy PASS text and matching symbols cannot manufacture a validation pass', async () => {
    const { context, get } = setup([{ data: result({ validation: undefined,
        compiled: '50 !', audit: '[PASS] 100% 合格' }) }]);
    await context.runCompile();
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'not_run');
    assert.match(get('invariantCapsules').textContent, /未校验/);
    assert.equal(get('invariantCapsules').children.some(e => e.className === 'inv-pill-ok'), false);
    assert.doesNotMatch(get('auditBox').textContent, /100%|PASS/);
});

test('a reported pass identifies its method and all token savings remain estimated', async () => {
    const { context, get, requests } = setup([{ data: result() }]);
    get('verifyCheck').checked = true;
    const source = '  keep exact whitespace\n\n  ';
    get('rawInput').value = source;
    await context.runCompile();
    assert.equal(JSON.parse(requests[0].options.body).text, source);
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'passed');
    assert.match(get('invariantCapsules').textContent, /模型对照通过/);
    assert.match(get('statsBadge').textContent, /估算 100→70/);
    assert.match(get('statsBadge').textContent, /估算减少 30\.0%/);
    assert.doesNotMatch(get('statsBadge').textContent, /实测|99%/);
    assert.equal(get('copyBtn').disabled, false);
});

test('starting a new request clears prior success; HTTP 422 stays failed and empty', async () => {
    let resolve;
    const delayed = new Promise(done => { resolve = done; });
    const { context, get } = setup([{ data: result() }, () => delayed]);
    get('verifyCheck').checked = true;
    await context.runCompile();
    const pending = context.runCompile();
    assert.equal(get('dslOutput').value, '');
    assert.equal(get('copyBtn').disabled, true);
    assert.equal(get('statsBadge').classList.contains('hidden'), true);
    assert.equal(get('cacheBadge').classList.contains('hidden'), true);
    assert.equal(get('invariantCapsules').dataset.validationStatus, undefined);
    resolve({ status: 422, data: { detail: '候选未通过',
        validation: validation('failed', 'local_checks', ['缺失订单字段']) } });
    await pending;
    assert.equal(get('dslOutput').value, '');
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'failed');
    assert.match(get('auditBox').textContent, /缺失订单字段/);
    assert.equal(get('auditBox').classList.contains('is-passed'), false);
});

test('HTTP 502 and non-JSON errors cannot retain successful validation', async () => {
    const { context, get } = setup([{ data: result() }, { status: 502,
        data: { detail: '审计服务不可用', validation: validation('error', 'llm_compare', ['上游超时']) } },
    { status: 502, raw: '<html>Bad Gateway</html>' }]);
    await context.runCompile();
    await context.runCompile();
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'error');
    assert.match(get('auditBox').textContent, /上游超时/);
    assert.equal(get('dslOutput').value, '');
    await context.runCompile();
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'error');
    assert.match(get('auditBox').textContent, /HTTP 502/);
});

test('strict mode rejects incomplete, malformed, and contradictory validation', async () => {
    const { context, get } = setup([
        { data: result({ validation: validation('passed', 'local_checks') }) },
        { data: result({ validation: validation('passed', 'none') }) },
        { data: result({ validation: validation('passed', 'llm_compare', ['missing constraint']) }) }
    ]);
    get('verifyCheck').checked = true;
    for (let i = 0; i < 3; i++) {
        await context.runCompile();
        assert.equal(get('dslOutput').value, '');
        assert.equal(get('copyBtn').disabled, true);
        assert.notEqual(get('invariantCapsules').dataset.validationStatus, 'passed');
    }
});

test('non-strict failed candidates remain visibly failed even when numbers and negation occur', async () => {
    const { context, get } = setup([{ data: result({ compiled: '50 !',
        validation: validation('failed', 'llm_compare', ['丢失删除禁止项']) }) }]);
    await context.runCompile();
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'failed');
    assert.equal(get('invariantCapsules').children.some(e => e.className === 'inv-pill-ok'), false);
    assert.match(get('auditBox').textContent, /丢失删除禁止项/);
});

test('upstream usage remains a whole-request count, never an inferred actual saving', () => {
    const { context } = setup();
    const legacy = context.formatProxyUsage({ real_prompt_tokens: 80, real_billing_ratio: '实测节省 99%' });
    assert.equal(legacy, '上游实际输入 80 tok（整次请求）');
    const current = context.formatProxyUsage({ real_prompt_tokens: 80,
        real_billing_ratio: '上游实际输入 80 tok（整次请求）；文本估算 100→60 tok' });
    assert.match(current, /已检查文本估算 100→60 tok/);
    assert.doesNotMatch(current, /实测|节省/);
    assert.equal(context.formatProxyUsage({ real_billing_ratio: '实测节省 99%' }), '');
    assert.match(context.formatTokenMeasurement({ original_tokens: 10, compiled_tokens: 6 }).text, /未确认计量来源/);
});

test('proxy decisions and reasons are rendered as text, and refresh errors are visible', async () => {
    const payload = { logs: [{ id: 'one', timestamp: '10:00:00', protocol: 'openai',
        decision: 'kept_original', reason: '候选未减少估算 Token', role: '<img src=x onerror=alert(1)>',
        original: 'source', compiled: 'source', real_prompt_tokens: 42,
        real_billing_ratio: '实测节省 99%' }] };
    const { context, get } = setup([{ data: payload }, new Error('offline')]);
    await context.loadLogs();
    assert.match(get('logsContainer').textContent, /已保留原文/);
    assert.match(get('logsContainer').textContent, /候选未减少估算 Token/);
    assert.match(get('logsContainer').textContent, /实际发送：原文/);
    assert.doesNotMatch(get('logsContainer').textContent, /实测节省/);
    await context.loadLogs();
    assert.equal(get('logsError').classList.contains('hidden'), false);
    assert.match(get('logsError').textContent, /offline.*当前记录可能过期/);
});
