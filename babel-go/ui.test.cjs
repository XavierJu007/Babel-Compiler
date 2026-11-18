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
    setAttribute(name, value) { this[name] = value; }
    querySelectorAll() { return []; }
    focus() {}
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
        confirm: () => true,
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

function domainPack(name, source = 'builtin') {
    return { schema_version: 1, name, label: name === 'common' ? '公共逻辑' : 'API 契约',
        version: 1, source, entries: [{ id: 'protocol', canonical: '接口契约',
            aliases: ['HTTP 方法'], triggers: ['POST'], use_when: ['原文规定接口方法时'],
            preserve: ['方法及路径'], do_not_merge_with: ['GET 与 POST'],
            symbols: [{ symbol: '∈', meaning: '集合成员' }],
            examples: [{ input: '缺少字段时拒绝请求', output: '缺少字段：拒绝请求' }] }] };
}

function domainPayload(domains) {
    return { domains, public_symbols: [{ symbol: '∈', meaning: '集合成员' }, { symbol: '!=', meaning: '不等于' }] };
}

test('domain selection is independent of the output preset and common is implicit', async () => {
    const { context, get, requests } = setup([
        { data: domainPayload([domainPack('common'), domainPack('api-contract')]) }, { data: result() }
    ]);
    await context.loadDomainPacks();
    const labels = get('domainChoices').children;
    assert.equal(labels[0].children[0].checked, true);
    assert.equal(labels[0].children[0].disabled, true);
    const input = labels[1].children[0];
    input.checked = true;
    input.onchange();
    get('modeSelect').value = 'en-terse';
    await context.runCompile();
    const body = JSON.parse(requests[1].options.body);
    assert.equal(body.mode, 'en-terse');
    assert.deepEqual(body.domains, ['api-contract']);
});

test('domain field edits, public symbols and examples survive entry navigation and save', async () => {
    const base = domainPack('api-contract');
    const saved = structuredClone(base);
    saved.source = 'global';
    const { context, get, requests } = setup([
        { data: domainPayload([domainPack('common'), base]) },
        { data: { success: true } }, { data: domainPayload([domainPack('common'), saved]) }
    ]);
    await context.loadDomainPacks();
    context.selectDomainPackForEdit('api-contract');
    context.updateDomainEntryField('canonical', '请求契约');
    context.updateDomainEntryField('preserve', '方法及路径\n字段大小写\n');
    context.addDomainSymbol();
    const symbolInputs = get('domainSymbolRows').children[1].children;
    symbolInputs[0].value = '!=';
    symbolInputs[0].onchange();
    assert.equal(get('domainSymbolRows').children[1].children[1].readOnly, true);
    assert.equal(get('domainSymbolRows').children[1].children[1].value, '不等于');
    context.addDomainExample();
    const example = get('domainExampleRows').children[1];
    example.children[0].children[1].value = '值不是 null 才处理';
    example.children[0].children[1].oninput();
    example.children[1].children[1].value = '处理前提：值 != null';
    example.children[1].children[1].oninput();
    context.addDomainEntry();
    context.updateDomainEntryField('canonical', '备用词条');
    context.updateDomainEntryField('use_when', '仅用于示例');
    get('domainEntryList').children[0].onclick();
    assert.equal(get('deCanonical').value, '请求契约');
    await context.saveDomainPack();
    const request = JSON.parse(requests[1].options.body);
    assert.equal(request.action, 'save');
    assert.equal(request.scope, 'global');
    assert.equal(request.domain.source, undefined);
    assert.equal(request.domain.entries.length, 2);
    assert.deepEqual(request.domain.entries[0].preserve, ['方法及路径', '字段大小写']);
    assert.deepEqual(request.domain.entries[0].symbols[1], { symbol: '!=', meaning: '不等于' });
    assert.equal(request.domain.entries[0].examples[1].output, '处理前提：值 != null');
    assert.match(get('domainStatus').textContent, /已保存并加载/);
});

test('domain editor refuses incomplete entries and never deletes a builtin', async () => {
    const { context, get, requests } = setup([{ data: domainPayload([domainPack('common')]) }]);
    await context.loadDomainPacks();
    context.selectDomainPackForEdit('common');
    assert.equal(get('deleteDomainBtn').disabled, true);
    await context.deleteDomainPack();
    context.updateDomainEntryField('use_when', '');
    await context.saveDomainPack();
    assert.equal(requests.length, 1);
    assert.match(get('domainStatus').textContent, /适用条件/);
    assert.match(get('domainStatus').className, /is-error/);
});

test('deleting a scoped override reloads the builtin and preserves domain selection', async () => {
    const { context, get, requests } = setup([
        { data: domainPayload([domainPack('common'), domainPack('api-contract', 'workspace')]) },
        { data: { success: true } },
        { data: domainPayload([domainPack('common'), domainPack('api-contract')]) }
    ]);
    await context.loadDomainPacks();
    const choice = get('domainChoices').children[1].children[0];
    choice.checked = true;
    choice.onchange();
    context.selectDomainPackForEdit('api-contract');
    assert.equal(get('dmScope').value, 'workspace');
    assert.equal(get('deleteDomainBtn').disabled, false);
    await context.deleteDomainPack();
    assert.deepEqual(JSON.parse(requests[1].options.body), { action: 'delete', scope: 'workspace', name: 'api-contract' });
    assert.equal(get('deleteDomainBtn').disabled, true);
    assert.match(get('domainSourceInfo').textContent, /内置/);
    assert.equal(get('domainChoices').children[1].children[0].checked, true);
});

test('unsupported or redefined symbols remain visible errors and cannot be saved', async () => {
    const modified = domainPack('api-contract');
    modified.entries[0].symbols = [{ symbol: 'A17', meaning: '任意业务段落' }, { symbol: '∈', meaning: '删除数据' }];
    const { context, get, requests } = setup([{ data: domainPayload([modified]) }]);
    await context.loadDomainPacks();
    context.selectDomainPackForEdit('api-contract');
    assert.match(get('domainSymbolRows').textContent, /不在目录中或含义与标准不一致/);
    await context.saveDomainPack();
    assert.equal(requests.length, 1);
    assert.match(get('domainStatus').textContent, /未支持的符号或自定义含义/);
});

test('a disappeared selected domain stays explicit instead of silently changing the compile request', async () => {
    const { context, get, requests } = setup([
        { data: domainPayload([domainPack('common'), domainPack('api-contract')]) },
        { data: domainPayload([domainPack('common')]) }, { status: 400, data: { detail: '领域不存在' } }
    ]);
    await context.loadDomainPacks();
    const choice = get('domainChoices').children[1].children[0];
    choice.checked = true;
    choice.onchange();
    await context.loadDomainPacks();
    assert.match(get('domainChoices').textContent, /api-contract（不可用）/);
    assert.match(get('domainSelectionHint').textContent, /不会静默忽略/);
    await context.runCompile();
    assert.deepEqual(JSON.parse(requests[2].options.body).domains, ['api-contract']);
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'error');
});
