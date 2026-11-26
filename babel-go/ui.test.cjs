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
    constructor(tagName = 'div') {
        this.tagName = tagName.toUpperCase();
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
    get options() { return this.children; }
    add(child) { this.appendChild(child); if (child.selected) this.value = child.value; }
    replaceChildren(...children) { this._text = ''; this.children = children; }
    setAttribute(name, value) { this[name] = value; }
    querySelectorAll() { return []; }
    focus() { this.focused = true; }
    select() { this.selected = true; }
}

function setup(responses = []) {
    const elements = new Map();
    const get = id => {
        if (!elements.has(id)) elements.set(id, new Element());
        return elements.get(id);
    };
    const requests = [];
    const storage = new Map();
    const context = vm.createContext({
        document: {
            getElementById: get, createElement: tagName => new Element(tagName),
            querySelectorAll: () => [], querySelector: () => null
        },
        Option: function(text, value, defaultSelected, selected) {
            const option = new Element('option');
            option.textContent = text;
            option.value = value;
            option.selected = selected;
            return option;
        },
        localStorage: { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) },
        console: { error: () => {} },
        window: { location: { origin: 'http://127.0.0.1:8080' } },
        fetch: async (url, options) => {
            requests.push({ url, options });
            let response = responses.shift();
            if (typeof response === 'function') response = await response(options);
            if (response instanceof Error) throw response;
            if (!response) throw new Error('Unexpected fetch');
            const status = response.status || 200;
            return {
                status,
                ok: status >= 200 && status < 300,
                headers: response.headers,
                body: response.body,
                text: async () => response.raw ?? JSON.stringify(response.data)
            };
        },
        setTimeout: () => {},
        confirm: () => true,
        navigator: { clipboard: { writeText: async () => {} } },
        TextDecoder,
        AbortController,
        alert: message => { throw new Error(`Unexpected alert: ${message}`); }
    });
    vm.runInContext(app, context);
    get('rawInput').value = '禁止删除订单；金额 > 50 元时审批。';
    for (const id of ['candidateBadge', 'statsBadge', 'cacheBadge', 'auditBox', 'invariantCapsules', 'logsError', 'dryRunBanner', 'proxyQuickstartPanel', 'stopBtn', 'compileProgressBar', 'operatorBar', 'routePreviewBadge', 'historyPanel', 'forceDeepBtn']) {
        get(id).className = 'hidden';
    }
    return { context, get, requests, storage };
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
            measurement_encoding: 'estimate', measurement_version: '2',
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

test('starting a new request clears prior success; HTTP 422 shows a failed candidate for inspection and copying', async () => {
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
    const candidate = '  订单状态→待确认\n';
    resolve({ status: 422, data: result({ compiled: candidate, cache_hit: false, detail: '候选未通过',
        validation: validation('failed', 'local_checks', ['缺失订单字段']) }) });
    await pending;
    assert.equal(get('dslOutput').value, candidate);
    assert.equal(get('dslOutput').dataset.candidateStatus, 'failed');
    assert.equal(get('copyBtn').disabled, false);
    assert.equal(get('copyBtn').textContent, '复制未通过候选');
    assert.match(get('candidateBadge').textContent, /未通过候选.*仅供检查/);
    assert.equal(get('candidateBadge').classList.contains('inv-pill-fail'), true);
    assert.match(get('statsBadge').textContent, /估算 100→70/);
    assert.equal(get('statsBadge').classList.contains('hidden'), false);
    assert.equal(get('cacheBadge').classList.contains('hidden'), true);
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'failed');
    assert.match(get('auditBox').textContent, /缺失订单字段/);
    assert.equal(get('auditBox').classList.contains('is-passed'), false);
    let resetLabel, copied;
    context.setTimeout = callback => { resetLabel = callback; };
    context.navigator.clipboard.writeText = async text => { copied = text; };
    await context.copyResult();
    assert.equal(copied, candidate);
    assert.equal(get('copyBtn').textContent, '已复制候选');
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'failed');
    assert.equal(get('candidateBadge').classList.contains('inv-pill-fail'), true);
    resetLabel();
    assert.equal(get('copyBtn').textContent, '复制未通过候选');
});

test('audit service errors retain the current candidate and diagnostics without presenting acceptance', async () => {
    const candidate = '<script>alert("candidate")</script>\n金额>50→审批';
    const { context, get } = setup([{ status: 502, data: result({ compiled: candidate, cache_hit: false,
        detail: '审计服务不可用', validation: validation('error', 'llm_compare', ['上游超时']) }) }]);
    get('verifyCheck').checked = true;
    await context.runCompile();
    assert.equal(get('dslOutput').value, candidate);
    assert.equal(get('dslOutput').dataset.candidateStatus, 'error');
    assert.equal(get('copyBtn').textContent, '复制未验收候选');
    assert.equal(get('copyBtn').disabled, false);
    assert.match(get('candidateBadge').textContent, /未验收候选.*仅供检查/);
    assert.equal(get('candidateBadge').classList.contains('inv-pill-fail'), true);
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'error');
    assert.match(get('auditBox').textContent, /上游超时/);
    assert.match(get('auditBox').textContent, /审计服务不可用/);
    assert.equal(get('auditBox').classList.contains('is-passed'), false);
    assert.match(get('statsBadge').textContent, /估算 100→70/);
});

test('candidate failures mentioning API Key must not obscure the candidate with a configuration dialog', async () => {
    const literalIssue = '未保留技术字面量: "API Key"';
    const readerIssue = '当前配置方案 [reader] 未设置 API Key';
    const readerError = blindValidation('error');
    readerError.issues = [readerIssue];
    readerError.stages[1].status = 'error';
    readerError.stages[1].issues = [readerIssue];
    readerError.stages[2].status = 'not_run';
    for (const tc of [
        { status: 422, detail: `[语义门禁拦截] ${literalIssue}`, validation: validation('failed', 'local_checks', [literalIssue]) },
        { status: 502, detail: readerIssue, validation: readerError }
    ]) {
        const { context, get } = setup([{ data: configFixture() }, { status: tc.status, data: result({
            compiled: '保留配置字段', detail: tc.detail, validation: tc.validation }) }]);
        await context.loadInitConfig();
        get('verifyCheck').checked = true;
        get('validationModeSelect').value = tc.validation.method === 'blind_read' ? 'blind' : 'compare';
        get('configModal').className = 'hidden';
        get('rawInput').value = '请逐字保留字符串 "API Key"。';
        await context.runCompile();
        assert.equal(get('dslOutput').value, '保留配置字段');
        assert.equal(get('invariantCapsules').dataset.validationStatus, tc.validation.status);
        assert.ok(get('auditBox').textContent.includes(tc.detail));
        assert.equal(get('configModal').classList.contains('hidden'), true);
        if (tc.validation.method === 'blind_read') {
            assert.match(get('auditBox').textContent, /独立阅读器：异常/);
            assert.match(get('auditBox').textContent, /原文对照：未执行/);
        }
    }
});

test('a local missing-key error without a candidate still opens model configuration', async () => {
    const { context, get } = setup([{ data: configFixture({ has_api_key: false }) }, {
        status: 400, data: { detail: '当前配置方案 [compiler] 未设置 API Key' }
    }]);
    await context.loadInitConfig();
    get('verifyCheck').checked = true;
    get('configModal').className = 'hidden';
    await context.runCompile();
    assert.equal(get('dslOutput').value, '');
    assert.equal(get('copyBtn').disabled, true);
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'error');
    assert.equal(get('configModal').classList.contains('hidden'), false);
    assert.equal(get('cfgProfileName').value, 'compiler');
});

test('error responses cannot manufacture acceptance or display absent or invalid token counts', async () => {
    for (const stats of [undefined, null, {}, { original_tokens: 100, compiled_tokens: -1 },
        { original_tokens: '100', compiled_tokens: 70 }, { original_tokens: 100.5, compiled_tokens: 70 },
        { original_tokens: 0, compiled_tokens: 0, measurement_kind: '', measurement_scope: '',
            measurement_encoding: '', measurement_source: '', measurement_version: '', compression_ratio: '' }]) {
        const { context, get } = setup([{ status: 502, data: result({ compiled: '当前未验收候选', stats,
            detail: '请求异常但错误地携带通过状态', validation: validation('passed') }) }]);
        get('verifyCheck').checked = true;
        await context.runCompile();
        assert.equal(get('dslOutput').value, '当前未验收候选');
        assert.equal(get('copyBtn').disabled, false);
        assert.equal(get('invariantCapsules').dataset.validationStatus, 'error');
        assert.equal(get('invariantCapsules').children.some(e => e.className === 'inv-pill-ok'), false);
        assert.equal(get('statsBadge').classList.contains('hidden'), true);
        assert.doesNotMatch(get('statsBadge').textContent, /→|减少|增加/);
    }
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
    assert.equal(get('copyBtn').disabled, true);
    assert.equal(get('candidateBadge').classList.contains('hidden'), true);
    assert.equal(get('statsBadge').classList.contains('hidden'), true);
    await context.runCompile();
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'error');
    assert.match(get('auditBox').textContent, /HTTP 502/);
    assert.equal(get('dslOutput').value, '');
    assert.equal(get('copyBtn').disabled, true);
});

test('errors without a valid current candidate clear a preceding failed candidate and its copy state', async () => {
    for (const compiled of [undefined, null, '', '   \n', 123]) {
        const { context, get } = setup([
            { status: 422, data: result({ compiled: '上一轮失败候选',
                validation: validation('failed', 'local_checks', ['上一轮缺失字段']) }) },
            { status: 422, data: { compiled, detail: '本次生成未返回候选',
                validation: validation('failed', 'local_checks', ['本次没有候选']) } }
        ]);
        get('verifyCheck').checked = true;
        await context.runCompile();
        assert.equal(get('dslOutput').value, '上一轮失败候选');
        await context.runCompile();
        assert.equal(get('dslOutput').value, '');
        assert.equal(get('dslOutput').dataset.candidateStatus, undefined);
        assert.equal(get('copyBtn').disabled, true);
        assert.equal(get('candidateBadge').classList.contains('hidden'), true);
        assert.equal(get('statsBadge').classList.contains('hidden'), true);
        assert.equal(get('cacheBadge').classList.contains('hidden'), true);
        assert.equal(get('invariantCapsules').dataset.validationStatus, 'failed');
        assert.match(get('auditBox').textContent, /本次没有候选/);
        assert.doesNotMatch(get('auditBox').textContent, /上一轮/);
    }
});

test('editing the source clears the previous candidate, counts, validation and copy state', async () => {
    for (const response of [{ data: result() }, { status: 422, data: result({
        validation: validation('failed', 'local_checks', ['缺少字段']) }) }]) {
        const { context, get } = setup([response]);
        get('verifyCheck').checked = true;
        await context.runCompile();
        assert.equal(get('dslOutput').value, result().compiled);
        assert.equal(get('copyBtn').disabled, false);
        get('rawInput').value = '允许删除订单，不需要审批。';
        context.updateInputCount();
        assert.equal(get('dslOutput').value, '');
        assert.equal(get('dslOutput').dataset.candidateStatus, undefined);
        assert.equal(get('candidateBadge').classList.contains('hidden'), true);
        assert.equal(get('statsBadge').classList.contains('hidden'), true);
        assert.equal(get('cacheBadge').classList.contains('hidden'), true);
        assert.equal(get('copyBtn').disabled, true);
        assert.equal(get('invariantCapsules').dataset.validationStatus, 'not_run');
        assert.match(get('auditBox').textContent, /原文已更改，请重新编译/);
    }
});

test('responses and errors for an edited source cannot become the current result', async () => {
    for (const response of [
        { data: result() },
        { status: 422, data: result({ compiled: '旧原文候选', detail: '旧原文校验失败', validation: validation('failed', 'local_checks', ['旧原文缺失字段']) }) },
        { status: 502, data: result({ compiled: '旧原文候选', detail: '旧请求超时', validation: validation('error', 'llm_compare', ['旧请求异常']) }) }
    ]) {
        let resolve;
        const delayed = new Promise(done => { resolve = done; });
        const { context, get, requests } = setup([() => delayed]);
        get('verifyCheck').checked = true;
        const submitted = get('rawInput').value;
        const pending = context.runCompile();
        get('rawInput').value = '允许删除订单，不需要审批。';
        context.updateInputCount();
        resolve(response);
        await pending;
        assert.equal(JSON.parse(requests[0].options.body).text, submitted);
        assert.equal(get('dslOutput').value, '');
        assert.equal(get('copyBtn').disabled, true);
        assert.equal(get('statsBadge').classList.contains('hidden'), true);
        assert.equal(get('invariantCapsules').dataset.validationStatus, 'not_run');
        assert.match(get('auditBox').textContent, /原文已更改，请重新编译/);
        assert.doesNotMatch(get('auditBox').textContent, /旧原文|旧请求/);
        assert.equal(get('runBtn').disabled, false);
        assert.equal(get('forceBtn').disabled, false);
    }
});

test('a late error candidate cannot overwrite a newer request or restore its buttons early', async () => {
    for (const changedSource of [false, true]) {
        for (const oldFinishesFirst of [false, true]) {
            let resolveOld, resolveNew;
            const oldResponse = new Promise(resolve => { resolveOld = resolve; });
            const newResponse = new Promise(resolve => { resolveNew = resolve; });
            const { context, get } = setup([() => oldResponse, () => newResponse]);
            get('verifyCheck').checked = true;
            const oldRequest = context.runCompile();
            if (changedSource) {
                get('rawInput').value = '新的原文';
                context.updateInputCount();
            }
            const newRequest = context.runCompile();
            const oldError = { status: 422, data: result({ compiled: '旧请求的失败候选', detail: '旧请求拒绝',
                validation: validation('failed', 'local_checks', ['旧请求缺少规则']) }) };
            if (oldFinishesFirst) {
                resolveOld(oldError);
                await oldRequest;
                assert.equal(get('dslOutput').value, '');
                assert.equal(get('runBtn').disabled, true);
                assert.equal(get('forceBtn').disabled, true);
                assert.equal(get('invariantCapsules').dataset.validationStatus, undefined);
            }
            resolveNew({ data: result({ compiled: '当前请求的候选' }) });
            await newRequest;
            if (!oldFinishesFirst) {
                resolveOld(oldError);
                await oldRequest;
            }
            assert.equal(get('dslOutput').value, '当前请求的候选');
            assert.equal(get('copyBtn').disabled, false);
            assert.equal(get('copyBtn').textContent, '复制结果');
            assert.equal(get('invariantCapsules').dataset.validationStatus, 'passed');
            assert.doesNotMatch(get('auditBox').textContent, /旧请求|已更改/);
            assert.equal(get('runBtn').disabled, false);
            assert.equal(get('forceBtn').disabled, false);
        }
    }
});

test('changed compile options and domains invalidate completed and in-flight results', async () => {
    const changes = [
        ['encoding', get => { get('tokenEncodingSelect').value = 'o200k_base'; }],
        ['validation mode', get => { get('validationModeSelect').value = 'compare'; }],
        ['reader', get => { get('readerProfileSelect').value = 'profile:reader'; }],
        ['strict gate', get => { get('verifyCheck').checked = false; }],
        ['domain', get => {
            const input = get('domainChoices').children[1].children[0];
            input.checked = true;
            input.onchange();
        }]
    ];
    for (const [name, change] of changes) {
        for (const inFlight of [false, true]) {
            let resolve;
            const delayed = new Promise(done => { resolve = done; });
            const { context, get, requests } = setup([
                { data: configFixture() },
                { data: domainPayload([domainPack('common'), domainPack('api-contract')]) },
                inFlight ? () => delayed : { data: result({ validation: blindValidation() }) }
            ]);
            await context.loadInitConfig();
            await context.loadDomainPacks();
            get('verifyCheck').checked = true;
            get('validationModeSelect').value = 'blind';
            const pending = context.runCompile();
            if (!inFlight) await pending;
            change(get);
            context.updateCompileOptionsHint();
            assert.equal(get('invariantCapsules').dataset.validationStatus, 'not_run', name);
            if (inFlight) {
                resolve({ status: 422, data: result({ compiled: '旧设置的失败候选',
                    validation: validation('failed', 'local_checks', ['旧设置下未通过']) }) });
                await pending;
            }
            assert.equal(JSON.parse(requests[2].options.body).profile, 'compiler');
            assert.equal(get('dslOutput').value, '', name);
            assert.equal(get('statsBadge').classList.contains('hidden'), true, name);
            assert.equal(get('copyBtn').disabled, true, name);
            assert.equal(get('invariantCapsules').dataset.validationStatus, 'not_run', name);
            assert.match(get('auditBox').textContent, /设置已更改，请重新编译/, name);
            assert.equal(get('runBtn').disabled, false, name);
        }
    }
});

test('profile switching pins each compile request and ignores a late old-profile error', async () => {
    let resolveCompile, resolveSwitch;
    const delayedCompile = new Promise(done => { resolveCompile = done; });
    const delayedSwitch = new Promise(done => { resolveSwitch = done; });
    const config = configFixture();
    config.profiles.reader.token_encoding = 'cl100k_base';
    const switched = structuredClone(config);
    switched.active_profile = 'reader';
    const { context, get, requests } = setup([
        { data: config }, () => delayedCompile, () => delayedSwitch,
        { data: result() }, { data: switched }
    ]);
    await context.loadInitConfig();
    get('verifyCheck').checked = true;
    const compiling = context.runCompile();
    const switching = context.quickSwitchProfile('reader');
    resolveCompile({ status: 502, data: result({ compiled: '旧方案候选', detail: '旧方案超时',
        validation: validation('error', 'llm_compare', ['旧方案异常']) }) });
    await compiling;
    assert.equal(JSON.parse(requests[1].options.body).profile, 'compiler');
    assert.equal(get('dslOutput').value, '');
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'not_run');
    assert.doesNotMatch(get('auditBox').textContent, /旧方案/);
    await context.runCompile();
    const nextRequest = JSON.parse(requests[3].options.body);
    assert.equal(nextRequest.profile, 'reader');
    assert.equal(nextRequest.token_encoding, 'cl100k_base');
    resolveSwitch({ data: { success: true } });
    await switching;
    assert.equal(get('headerProfileSelect').value, 'reader');
});

test('copy reports success only after the exact result reaches the clipboard', async () => {
    const { context, get } = setup();
    const text = '  保留空格\n\n和换行\n';
    get('dslOutput').value = text;
    let resolve, copied, resetLabel;
    context.navigator.clipboard.writeText = value => {
        copied = value;
        return new Promise(done => { resolve = done; });
    };
    context.setTimeout = callback => { resetLabel = callback; };
    const pending = context.copyResult();
    assert.equal(copied, text);
    assert.equal(get('copyBtn').textContent, '复制中...');
    assert.equal(get('copyBtn').disabled, true);
    resolve();
    await pending;
    assert.equal(get('copyBtn').textContent, '已复制');
    assert.equal(get('copyBtn').disabled, false);
    resetLabel();
    assert.equal(get('copyBtn').textContent, '复制结果');
});

test('clipboard denial or absence preserves and selects the result for manual copying', async () => {
    for (const clipboard of [undefined, { writeText: async () => { throw new Error('NotAllowedError'); } }]) {
        const { context, get } = setup();
        const text = '  可手动复制的结果\n';
        get('dslOutput').value = text;
        context.navigator.clipboard = clipboard;
        await context.copyResult();
        assert.equal(get('dslOutput').value, text);
        assert.equal(get('dslOutput').focused, true);
        assert.equal(get('dslOutput').selected, true);
        assert.equal(get('copyBtn').textContent, '复制失败，请手动复制');
        assert.equal(get('copyBtn').disabled, false);
        context.navigator.clipboard = { writeText: async value => { assert.equal(value, text); } };
        await context.copyResult();
        assert.equal(get('copyBtn').textContent, '已复制');
    }
});

test('a late clipboard result or timer cannot relabel a newer compile result', async () => {
    const { context, get } = setup([{ data: result() }, { data: result({ compiled: '新候选' }) }]);
    get('verifyCheck').checked = true;
    await context.runCompile();
    let resolve;
    context.navigator.clipboard.writeText = () => new Promise(done => { resolve = done; });
    const pending = context.copyResult();
    await context.runCompile();
    resolve();
    await pending;
    assert.equal(get('dslOutput').value, '新候选');
    assert.equal(get('copyBtn').disabled, false);
    assert.equal(get('copyBtn').textContent, '复制结果');

    let resetLabel;
    context.navigator.clipboard.writeText = async () => {};
    context.setTimeout = callback => { resetLabel = callback; };
    await context.copyResult();
    context.resetCompileResult();
    get('copyBtn').textContent = '新操作';
    resetLabel();
    assert.equal(get('copyBtn').textContent, '新操作');
});

test('strict mode preserves candidates with incomplete, malformed, and contradictory validation as unaccepted', async () => {
    const { context, get } = setup([
        { data: result({ validation: undefined }) },
        { data: result({ validation: validation('passed', 'local_checks') }) },
        { data: result({ validation: validation('passed', 'none') }) },
        { data: result({ validation: validation('passed', 'llm_compare', ['missing constraint']) }) }
    ]);
    get('verifyCheck').checked = true;
    for (let i = 0; i < 4; i++) {
        await context.runCompile();
        assert.equal(get('dslOutput').value, result().compiled);
        assert.equal(get('copyBtn').disabled, false);
        assert.equal(get('copyBtn').textContent, '复制未验收候选');
        assert.match(get('candidateBadge').textContent, /未验收候选.*仅供检查/);
        assert.notEqual(get('invariantCapsules').dataset.validationStatus, 'passed');
        assert.equal(get('auditBox').classList.contains('is-passed'), false);
    }
});

test('non-strict failed candidates remain visibly failed even when numbers and negation occur', async () => {
    const { context, get } = setup([{ data: result({ compiled: '50 !',
        validation: validation('failed', 'llm_compare', ['丢失删除禁止项']) }) }]);
    await context.runCompile();
    assert.equal(get('dslOutput').value, '50 !');
    assert.equal(get('copyBtn').textContent, '复制未通过候选');
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

function blindValidation(status = 'passed') {
    return { status, method: 'blind_read', policy_version: 'semantic-blind-v1', issues: [], stages: [
        { name: 'local_checks', status: 'completed', policy_version: 'semantic-gate-v2', issues: [] },
        { name: 'reader', status: 'completed', model: 'reader-model', policy_version: 'blind-reader-v1', issues: [],
            duration_ms: 12, reading: { schema_version: 1, entities: ['订单'], relations: [], ambiguities: [] } },
        { name: 'comparator', status: 'passed', model: 'compiler-model', policy_version: 'blind-comparator-v1', issues: [], duration_ms: 16 }
    ] };
}

function configFixture(overrides = {}) {
    return { active_profile: 'compiler', profiles: {
        compiler: { name: 'compiler', model: 'qwen-test', base_url: 'http://127.0.0.1:9000/v1',
            has_api_key: true, token_encoding: 'estimate', validation_mode: 'compare', reader_profile: '', ...overrides },
        reader: { name: 'reader', model: 'reader-model', has_api_key: true, token_encoding: 'estimate', validation_mode: 'compare', reader_profile: '' }
    }, token_encodings: ['estimate', 'cl100k_base', 'o200k_base'] };
}

test('profile saves never preserve credentials in browser backups', async () => {
    for (const profileName of ['compiler', 'fresh']) {
        const saved = configFixture();
        if (profileName === 'fresh') {
            saved.profiles.fresh = { ...saved.profiles.compiler, name: 'fresh', has_api_key: false };
            saved.active_profile = 'fresh';
        }
        const { context, get, requests, storage } = setup([
            { data: configFixture() }, { data: { success: true } }, { data: saved }
        ]);
        storage.set('babel_dsl_backup_v2', JSON.stringify({ profile_name: 'compiler', api_key: 'fixture-old-key' }));
        await context.loadInitConfig();
        get('cfgProfileName').value = profileName;
        get('cfgApiKey').value = profileName === 'compiler' ? 'fixture-new-key' : '';
        await context.saveProfileConfig();
        const request = JSON.parse(requests[1].options.body);
        assert.equal(request.profile_name, profileName);
        assert.equal(request.api_key, profileName === 'compiler' ? 'fixture-new-key' : '');
        const backup = JSON.parse(storage.get('babel_dsl_backup_v2'));
        assert.equal(backup.profile_name, profileName);
        assert.equal(Object.hasOwn(backup, 'api_key'), false);
        assert.doesNotMatch(storage.get('babel_dsl_backup_v2'), /fixture-(?:old|new)-key/);
        assert.equal(requests.length, 3, 'saving a new blank-key profile must not trigger backup key restoration');
        assert.equal(get('headerProfileSelect').value, profileName);
    }
});

test('legacy browser credentials are removed and cannot restore or redirect a profile', async () => {
    for (const active of ['compiler', 'reader']) {
        const config = configFixture();
        config.active_profile = active;
        config.profiles[active].has_api_key = false;
        const { context, get, requests, storage } = setup([{ data: config }]);
        storage.set('babel_dsl_backup_v2', JSON.stringify({
            profile_name: 'compiler', api_key: 'fixture-old-key', base_url: 'https://untrusted.invalid'
        }));
        await context.loadInitConfig();
        assert.equal(requests.length, 1, 'browser state must not write server configuration');
        assert.equal(get('headerProfileSelect').value, active);
        assert.match(get('headerModelBadge').textContent, /缺Key/);
        assert.equal(Object.hasOwn(JSON.parse(storage.get('babel_dsl_backup_v2')), 'api_key'), false);
    }
});

test('malformed browser backup cannot prevent loading server configuration', async () => {
    const { context, get, requests, storage } = setup([{ data: configFixture() }]);
    storage.set('babel_dsl_backup_v2', '{invalid');
    await context.loadInitConfig();
    assert.equal(requests.length, 1);
    assert.equal(get('headerProfileSelect').value, 'compiler');
});

test('clearing logs uses the protected JSON request boundary', async () => {
    const { context, requests } = setup([{ data: { status: 'cleared' } }, { data: { logs: [] } }]);
    await context.clearLogs();
    assert.equal(requests[0].options.method, 'DELETE');
    assert.equal(requests[0].options.headers['Content-Type'], 'application/json');
});

test('connection tests use the same default profile name as saving a blank-name form', async () => {
    const { context, get, requests } = setup([
        { data: configFixture() }, { data: { ok: false, detail: 'missing key' } }
    ]);
    await context.loadInitConfig();
    get('cfgProfileName').value = '  ';
    get('cfgApiKey').value = '';
    await context.testModelConnection();
    const request = JSON.parse(requests[1].options.body);
    assert.equal(request.profile_name, 'default');
    assert.equal(request.api_key, '');
    assert.match(get('cfgTestResult').textContent, /missing key/);
});

test('model request URLs retain custom paths, trailing slashes and queries when loaded, tested and saved', async () => {
    for (const url of [
        'http://127.0.0.1:9000/v1',
        'https://gateway.example/custom/compile/',
        'https://gateway.example/custom/compile/?api-version=2025-01-01&route=%2Fv1%2F'
    ]) {
        const config = configFixture({ base_url: url });
        const { context, get, requests, storage } = setup([
            { data: config }, { data: { ok: true, detail: 'connected', target_url: url } },
            { data: { success: true } }, { data: config }
        ]);
        await context.loadInitConfig();
        assert.equal(get('cfgBaseUrl').value, url);
        assert.match(get('cfgTestResult').textContent, /\/v1 基址自动补 \/chat\/completions/);
        get('cfgBaseUrl').value = `  ${url}  `;
        await context.testModelConnection();
        assert.equal(JSON.parse(requests[1].options.body).base_url, url);
        await context.saveProfileConfig();
        assert.equal(JSON.parse(requests[2].options.body).base_url, url);
        assert.equal(JSON.parse(storage.get('babel_dsl_backup_v2')).base_url, url);
        assert.equal(get('cfgBaseUrl').value, url);
    }
});

test('provider buttons explicitly fill complete request endpoints without a general URL suffix rule', () => {
    const endpoints = new Map([
        ['deepseek', 'https://api.deepseek.com/chat/completions'],
        ['openai', 'https://api.openai.com/v1/chat/completions'],
        ['openrouter', 'https://openrouter.ai/api/v1/chat/completions'],
        ['ollama', 'http://127.0.0.1:11434/v1/chat/completions']
    ]);
    const handlers = [...html.matchAll(/onclick="(applyPreset\([^"\n]+\))"/g)].map(match => match[1]);
    assert.equal(handlers.length, endpoints.size);
    const { context, get } = setup();
    for (const handler of handlers) {
        vm.runInContext(handler, context);
        const name = get('cfgProfileName').value;
        assert.equal(get('cfgBaseUrl').value, endpoints.get(name), name);
        endpoints.delete(name);
    }
    assert.equal(endpoints.size, 0);
    context.applyPreset('custom', 'https://gateway.example/my-route/?a=1', 'custom-model');
    assert.equal(get('cfgBaseUrl').value, 'https://gateway.example/my-route/?a=1');
    assert.doesNotMatch(html, /末尾请带|中转站通常需要带|探测 \/chat\/completions/);
    assert.match(html, /上游服务基址（透明代理转发）/);
    assert.match(html, /OpenAI 留空复用编译请求 URL；Anthropic 需填写/);
    assert.doesNotMatch(html, /留空则默认转发至上方 Base URL/);
});

test('profile token encoding and independent reader choices load, save, and persist', async () => {
    const saved = configFixture({ token_encoding: 'o200k_base', validation_mode: 'blind', reader_profile: 'reader' });
    const { context, get, requests, storage } = setup([
        { data: configFixture() }, { data: { success: true } }, { data: saved }
    ]);
    await context.loadInitConfig();
    assert.equal(get('cfgTokenEncoding').value, 'estimate');
    assert.equal(get('cfgModel').value, 'qwen-test');
    assert.equal(get('cfgReaderProfile').options.some(option => option.value === 'reader'), true);
    get('cfgTokenEncoding').value = 'o200k_base';
    get('cfgValidationMode').value = 'blind';
    get('cfgReaderProfile').value = 'reader';
    context.updateConfigValidationHint();
    assert.equal(get('cfgReaderProfile').disabled, false);
    assert.match(get('cfgValidationHint').textContent, /增加一次阅读器调用/);
    await context.saveProfileConfig();
    const body = JSON.parse(requests[1].options.body);
    assert.equal(body.token_encoding, 'o200k_base');
    assert.equal(body.validation_mode, 'blind');
    assert.equal(body.reader_profile, 'reader');
    const backup = JSON.parse(storage.get('babel_dsl_backup_v2'));
    assert.equal(backup.reader_profile, 'reader');
    assert.equal(backup.token_encoding, 'o200k_base');
    assert.equal(backup.validation_mode, 'blind');
    assert.equal(get('cfgReaderProfile').value, 'reader');
    assert.equal(context.selectedCompileOptions().effective_reader, 'reader');
});

test('compile overrides preserve encoding and omitted versus same-profile reader semantics', async () => {
    const { context, get, requests } = setup([
        { data: configFixture({ token_encoding: 'cl100k_base', validation_mode: 'blind', reader_profile: 'reader' }) },
        { data: result({ validation: blindValidation() }) }, { data: result() },
        { data: result({ validation: blindValidation() }) }
    ]);
    await context.loadInitConfig();
    get('verifyCheck').checked = true;
    await context.runCompile();
    const inherited = JSON.parse(requests[1].options.body);
    assert.equal(inherited.token_encoding, 'cl100k_base');
    assert.equal(inherited.validation_mode, 'blind');
    assert.equal(Object.hasOwn(inherited, 'reader_profile'), false);

    get('tokenEncodingSelect').value = 'estimate';
    get('validationModeSelect').value = 'compare';
    get('readerProfileSelect').value = 'same';
    await context.runCompile();
    const same = JSON.parse(requests[2].options.body);
    assert.equal(same.token_encoding, 'estimate');
    assert.equal(same.validation_mode, 'compare');
    assert.equal(same.reader_profile, '');
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'passed');

    get('tokenEncodingSelect').value = 'o200k_base';
    get('validationModeSelect').value = 'blind';
    get('readerProfileSelect').value = 'profile:reader';
    await context.runCompile();
    const named = JSON.parse(requests[3].options.body);
    assert.equal(named.token_encoding, 'o200k_base');
    assert.equal(named.reader_profile, 'reader');
});

test('strict blind validation requires the correct method and both completed stages', async () => {
    const missingComparator = blindValidation();
    missingComparator.stages.pop();
    const readerOnly = blindValidation();
    readerOnly.stages[2].status = 'not_run';
    const contradictory = blindValidation();
    contradictory.stages[1].status = 'failed';
    contradictory.stages[1].issues = ['无法独立解释 A17'];
    const wrongPolicy = blindValidation();
    wrongPolicy.policy_version = 'semantic-gate-v2';
    const { context, get } = setup([
        validation('passed'), missingComparator, readerOnly, contradictory, wrongPolicy
    ].map(value => ({ data: result({ validation: value }) })));
    get('verifyCheck').checked = true;
    get('validationModeSelect').value = 'blind';
    for (let i = 0; i < 5; i++) {
        await context.runCompile();
        assert.equal(get('dslOutput').value, result().compiled);
        assert.equal(get('copyBtn').disabled, false);
        assert.equal(get('copyBtn').textContent, '复制未验收候选');
        assert.notEqual(get('invariantCapsules').dataset.validationStatus, 'passed');
    }
});

test('blind pass requires all three stages with the current policies and exact statuses', async () => {
    const cases = [];
    for (const name of ['local_checks', 'reader', 'comparator']) {
        const missing = blindValidation();
        missing.stages = missing.stages.filter(stage => stage.name !== name);
        cases.push({ name: `missing ${name}`, validation: missing });
        const stale = blindValidation();
        stale.stages.find(stage => stage.name === name).policy_version += '-old';
        cases.push({ name: `stale ${name}`, validation: stale });
        const incomplete = blindValidation();
        incomplete.stages.find(stage => stage.name === name).status = 'not_run';
        cases.push({ name: `incomplete ${name}`, validation: incomplete });
    }
    const { context, get } = setup(cases.map(tc => ({ data: result({ validation: tc.validation }) })));
    get('verifyCheck').checked = true;
    get('validationModeSelect').value = 'blind';
    for (const tc of cases) {
        await context.runCompile();
        assert.equal(get('dslOutput').value, result().compiled, tc.name);
        assert.equal(get('copyBtn').disabled, false, tc.name);
        assert.equal(get('copyBtn').textContent, '复制未验收候选', tc.name);
        assert.equal(get('invariantCapsules').dataset.validationStatus, 'error', tc.name);
        assert.equal(get('auditBox').classList.contains('is-passed'), false, tc.name);
    }
});

test('blind pass displays stage models with reading folded, and cannot pass compare mode', async () => {
    const { context, get } = setup([{ data: result({ validation: blindValidation() }) },
        { data: result({ validation: blindValidation() }) }]);
    get('verifyCheck').checked = true;
    get('validationModeSelect').value = 'blind';
    await context.runCompile();
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'passed');
    assert.match(get('invariantCapsules').textContent, /独立盲读对照通过/);
    assert.match(get('auditBox').textContent, /独立阅读器：已完成 · reader-model/);
    assert.match(get('auditBox').textContent, /原文对照：通过 · compiler-model/);
    const details = get('auditBox').children.find(child => child.tagName === 'DETAILS');
    assert.ok(details);
    assert.ok(!details.open);
    get('validationModeSelect').value = 'compare';
    await context.runCompile();
    assert.equal(get('dslOutput').value, result().compiled);
    assert.equal(get('copyBtn').textContent, '复制未验收候选');
    assert.notEqual(get('invariantCapsules').dataset.validationStatus, 'passed');
});

test('unchecked strict validation stays unverified even if a prior blind pass is returned', async () => {
    const { context, get, requests } = setup([{ data: result({ validation: blindValidation() }) }]);
    get('validationModeSelect').value = 'blind';
    context.updateCompileOptionsHint();
    assert.equal(get('readerProfileSelect').disabled, true);
    assert.match(get('compileOptionsHint').textContent, /严格校验未开启/);
    await context.runCompile();
    assert.equal(JSON.parse(requests[0].options.body).strict_gate, false);
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'not_run');
    assert.match(get('auditBox').textContent, /未开启严格校验/);
    assert.equal(get('auditBox').classList.contains('is-passed'), false);
});

test('reader service errors clear a preceding blind success and retain stage diagnostics', async () => {
    const failed = blindValidation('error');
    failed.issues = ['独立阅读服务异常'];
    failed.stages[1].status = 'error';
    failed.stages[1].issues = ['reader timeout'];
    failed.stages[2].status = 'not_run';
    const { context, get } = setup([{ data: result({ validation: blindValidation() }) },
        { status: 502, data: { detail: '阅读器超时', validation: failed } }]);
    get('verifyCheck').checked = true;
    get('validationModeSelect').value = 'blind';
    await context.runCompile();
    await context.runCompile();
    assert.equal(get('dslOutput').value, '');
    assert.equal(get('statsBadge').classList.contains('hidden'), true);
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'error');
    assert.match(get('auditBox').textContent, /独立阅读器：异常/);
    assert.match(get('auditBox').textContent, /原文对照：未执行/);
});

test('exact measurements name their encoding, scope, library and version without billing claims', () => {
    const { context } = setup();
    const stats = { original_tokens: 30, compiled_tokens: 20, measurement_kind: 'exact',
        measurement_scope: 'text', measurement_source: 'tiktoken-go/tokenizer',
        measurement_encoding: 'o200k_base', measurement_version: 'v0.3.0' };
    const display = context.formatTokenMeasurement(stats);
    assert.match(display.text, /文本 Token 精确 \[o200k_base\] 30→20/);
    assert.match(display.text, /tiktoken-go\/tokenizer v0.3.0/);
    assert.match(display.title, /范围：text/);
    assert.match(display.title, /不代表.*账单/);
    assert.doesNotMatch(display.text, /估算|实测|账单/);
    for (const corrupt of [{ measurement_encoding: 'qwen' }, { measurement_version: '' }, { measurement_scope: 'request' }, { measurement_source: 'unknown' }]) {
        assert.match(context.formatTokenMeasurement({ ...stats, ...corrupt }).text, /未确认计量来源/);
    }
    const item = { ratio: '文本 Token（所选编码精确计数: o200k_base）30→20 (减少33.3%)',
        measurement_kind: 'exact', measurement_encoding: 'o200k_base', real_prompt_tokens: 80,
        real_billing_ratio: '上游实际输入 80 tok（整次请求）；已检查文本估算 50→40 tok' };
    assert.equal(context.formatProxyMeasurement(item), '文本精确 [o200k_base] 30→20 tok');
    assert.match(context.formatProxyUsage(item), /上游实际输入 80.*文本估算 50→40/);
    assert.doesNotMatch(context.formatProxyUsage(item), /精确|节省/);
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

test('domain copy preserves current edits as a separate unsaved dictionary', async () => {
    const base = domainPack('api-contract', 'workspace');
    base.file_path = '/fixture/domains/api-contract.json';
    base.version = 9;
    const { context, get, requests } = setup([{ data: domainPayload([
        domainPack('common'), base, domainPack('api-contract-copy', 'global'),
        domainPack('api-contract-copy-2', 'workspace')
    ]) }]);
    await context.loadDomainPacks();
    const choice = get('domainChoices').children[1].children[0];
    choice.checked = true;
    choice.onchange();
    context.selectDomainPackForEdit('api-contract');
    assert.equal(get('copyDomainBtn').disabled, false);
    get('dmLabel').value = '我的接口词典';
    get('dmVersion').value = '12';
    context.updateDomainEntryField('canonical', '订单请求契约');
    context.updateDomainEntryField('preserve', '方法及路径\n字段大小写');
    const exampleOutput = get('domainExampleRows').children[0].children[1].children[1];
    exampleOutput.value = '  技术文本中的空格\n不能丢失\n';
    exampleOutput.oninput();
    context.confirm = () => { throw new Error('copy must preserve edits, not discard them'); };
    context.copyDomainPack();
    const copy = JSON.parse(get('domainJSON').value);
    assert.equal(copy.name, 'api-contract-copy-3');
    assert.equal(copy.label, '我的接口词典（副本）');
    assert.equal(copy.version, 1);
    assert.equal(copy.source, undefined);
    assert.equal(copy.file_path, undefined);
    assert.equal(copy.entries[0].canonical, '订单请求契约');
    assert.deepEqual(copy.entries[0].preserve, ['方法及路径', '字段大小写']);
    assert.deepEqual(copy.entries[0].symbols, base.entries[0].symbols);
    assert.equal(copy.entries[0].examples[0].output, '  技术文本中的空格\n不能丢失\n');
    assert.equal(get('dmScope').value, 'workspace');
    assert.equal(get('deleteDomainBtn').disabled, true);
    assert.equal(choice.checked, true);
    assert.equal(requests.length, 1); // Copying sends no save request.
    assert.match(get('domainStatus').textContent, /新草稿/);
    context.updateDomainEntryField('canonical', '只修改副本');
    context.confirm = () => true;
    context.selectDomainPackForEdit('api-contract');
    assert.equal(get('dmLabel').value, base.label);
    assert.equal(get('deCanonical').value, base.entries[0].canonical);
    assert.equal(get('domainExampleRows').children[0].children[1].children[1].value,
        base.entries[0].examples[0].output);
});

test('domain copy rejects occupied names and saves only under a new identity', async () => {
    const base = domainPack('api-contract');
    const copy = domainPack('my-api', 'workspace');
    const { context, get, requests } = setup([
        { data: domainPayload([domainPack('common'), base]) },
        { data: { status: 'saved' } }, { data: domainPayload([domainPack('common'), base, copy]) }
    ]);
    await context.loadDomainPacks();
    context.selectDomainPackForEdit('api-contract');
    context.copyDomainPack();
    get('dmName').value = 'api-contract';
    await context.saveDomainPack();
    assert.equal(requests.length, 1);
    assert.match(get('domainStatus').textContent, /未占用的领域标识/);
    get('dmName').value = 'my-api';
    get('dmScope').value = 'workspace';
    await context.saveDomainPack();
    const payload = JSON.parse(requests[1].options.body);
    assert.equal(payload.action, 'save');
    assert.equal(payload.scope, 'workspace');
    assert.equal(payload.domain.name, 'my-api');
    assert.deepEqual(payload.domain.entries, base.entries);
    assert.equal(Object.hasOwn(payload.domain, 'source'), false);
    assert.equal(Object.hasOwn(payload.domain, 'file_path'), false);
    assert.equal(get('dmName').value, 'my-api');
    assert.match(get('domainStatus').textContent, /已保存并加载/);
    assert.equal(get('domainChoices').children[2].children[0].checked, false);
});

test('domain copy keeps maximum-length identifiers valid and refuses changes during save', async () => {
    const base = domainPack('a'.repeat(64));
    base.label = '字'.repeat(120);
    const occupied = domainPack('a'.repeat(59) + '-copy', 'global');
    const { context, get, requests } = setup([{ data: domainPayload([base, occupied]) }]);
    await context.loadDomainPacks();
    context.selectDomainPackForEdit(base.name);
    context.setDomainBusy(true);
    context.copyDomainPack();
    assert.equal(get('dmName').value, base.name);
    context.setDomainBusy(false);
    context.copyDomainPack();
    assert.equal(get('dmName').value, 'a'.repeat(57) + '-copy-2');
    assert.match(get('dmName').value, /^[a-z][a-z0-9_-]{0,63}$/);
    assert.equal(Array.from(get('dmLabel').value).length, 120);
    assert.equal(requests.length, 1);
});

test('builtin professional domains are marked as examples without relabelling user dictionaries', async () => {
    const { context, get } = setup([{ data: domainPayload([
        domainPack('common'), domainPack('api-contract'), domainPack('my-api', 'global')
    ]) }]);
    await context.loadDomainPacks();
    const choices = get('domainChoices').children;
    assert.doesNotMatch(choices[0].textContent, /示例/);
    assert.match(choices[1].textContent, /示例/);
    assert.doesNotMatch(choices[2].textContent, /示例/);
    context.selectDomainPackForEdit('api-contract');
    assert.match(get('domainSourceInfo').textContent, /内置示例/);
    context.selectDomainPackForEdit('my-api');
    assert.doesNotMatch(get('domainSourceInfo').textContent, /示例/);
});

test('compilation uses the fixed backend policy and preserves explicit domain selection', async () => {
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
    await context.runCompile();
    const body = JSON.parse(requests[1].options.body);
    assert.equal(Object.hasOwn(body, 'mode'), false);
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

test('reloading blocks draft edits until completion and releases controls after failure', async () => {
    for (const fails of [false, true]) {
        let resolve;
        const delayed = new Promise(done => { resolve = done; });
        const original = domainPack('api-contract');
        const refreshed = structuredClone(original);
        refreshed.entries[0].canonical = '磁盘中的新术语';
        const { context, get, requests } = setup([
            { data: domainPayload([original]) }, () => delayed,
            { data: domainPayload([refreshed]) }
        ]);
        await context.loadDomainPacks();
        context.selectDomainPackForEdit('api-contract');
        context.updateDomainEntryField('canonical', '重载前的草稿');
        const draft = get('domainJSON').value;
        const controls = [get('deCanonical'), get('dmName'), get('saveDomainBtn')];
        get('domainModal').querySelectorAll = () => controls;
        const pending = context.reloadDomainLibrary();
        assert.equal(controls.every(control => control.disabled), true);
        context.updateDomainEntryField('canonical', '重载期间不能接收的编辑');
        context.prepareNewDomain();
        assert.equal(get('domainJSON').value, draft);
        resolve(fails ? { status: 502, data: { detail: 'reload offline' } } : { data: { success: true } });
        await pending;
        assert.equal(controls.every(control => !control.disabled), true);
        if (fails) {
            assert.equal(get('domainJSON').value, draft);
            assert.match(get('domainStatus').textContent, /重载失败.*reload offline/);
            assert.equal(requests.length, 2);
        } else {
            assert.equal(get('deCanonical').value, '磁盘中的新术语');
            assert.match(get('domainStatus').textContent, /已从磁盘重载/);
        }
    }
});

test('committed domain saves and deletes distinguish refresh failure from write failure', async () => {
    for (const action of ['save', 'delete']) {
        for (const committed of [false, true]) {
            const { context, get, requests } = setup([
                { data: domainPayload([domainPack('api-contract', 'workspace')]) },
                committed ? { data: { success: true } } : { status: 500, data: { detail: 'write failed' } },
                new Error('GET refresh offline')
            ]);
            await context.loadDomainPacks();
            context.selectDomainPackForEdit('api-contract');
            context.updateDomainEntryField('canonical', '保留当前草稿');
            const draft = get('domainJSON').value;
            await (action === 'save' ? context.saveDomainPack() : context.deleteDomainPack());
            assert.equal(get('domainJSON').value, draft);
            assert.equal(get('domainStatus').classList.contains('is-passed'), false);
            const message = get('domainStatus').textContent;
            if (committed) {
                assert.match(message, action === 'save' ? /领域已保存，但刷新未完成/ : /覆盖已删除，但刷新未完成/);
                assert.match(message, /GET refresh offline/);
                assert.doesNotMatch(message, /^(保存|删除)失败/);
                assert.equal(requests.length, 3);
            } else {
                assert.match(message, action === 'save' ? /^保存失败/ : /^删除失败/);
                assert.match(message, /write failed/);
                assert.equal(requests.length, 2);
            }
        }
    }
});

test('a committed dictionary reload invalidates old results even when its refresh fails', async () => {
    const common = domainPack('common');
    const reloaded = structuredClone(common);
    reloaded.entries[0].canonical = '服务端重载后的术语';
    const { context, get, requests } = setup([
        { data: domainPayload([common]) }, { data: result() },
        { data: domainPayload([reloaded]) }, new Error('GET refresh offline')
    ]);
    await context.loadDomainPacks();
    context.selectDomainPackForEdit('common');
    const draft = get('domainJSON').value;
    get('verifyCheck').checked = true;
    await context.runCompile();
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'passed');
    assert.equal(get('copyBtn').disabled, false);
    await context.reloadDomainLibrary();
    assert.deepEqual(JSON.parse(requests[2].options.body), { action: 'reload' });
    assert.equal(get('domainJSON').value, draft);
    assert.match(get('domainStatus').textContent, /词典已重载，但列表刷新未完成.*GET refresh offline.*当前草稿已保留/);
    assert.doesNotMatch(get('domainStatus').textContent, /^重载失败/);
    assert.equal(get('dslOutput').value, '');
    assert.equal(get('copyBtn').disabled, true);
    assert.equal(get('statsBadge').classList.contains('hidden'), true);
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'not_run');
    assert.match(get('auditBox').textContent, /设置已更改，请重新编译/);
});

test('dictionary refresh invalidates results only when an effective selected dictionary changes', async () => {
    const common = domainPack('common');
    const selected = domainPack('api-contract');
    const unused = domainPack('my-api', 'global');
    const changedUnused = structuredClone(unused);
    changedUnused.entries[0].canonical = '未选词典的新术语';
    const changedSelected = structuredClone(selected);
    changedSelected.entries[0].canonical = '所选词典的新术语';
    const { context, get } = setup([
        { data: domainPayload([common, selected, unused]) }, { data: result() },
        { data: domainPayload([common, selected, changedUnused]) },
        { data: domainPayload([common, changedSelected, changedUnused]) }
    ]);
    await context.loadDomainPacks();
    const input = get('domainChoices').children[1].children[0];
    input.checked = true;
    input.onchange();
    get('verifyCheck').checked = true;
    await context.runCompile();
    await context.loadDomainPacks();
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'passed');
    assert.equal(get('copyBtn').disabled, false);
    await context.loadDomainPacks();
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'not_run');
    assert.equal(get('dslOutput').value, '');
    assert.equal(get('statsBadge').classList.contains('hidden'), true);
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

function findElements(root, predicate) {
    const hits = [];
    const walk = node => {
        if (!node) return;
        if (predicate(node)) hits.push(node);
        for (const child of node.children || []) walk(child);
    };
    walk(root);
    return hits;
}

test('empty proxy logs render 4 quickstart recipes without placeholder text', async () => {
    const { context, get } = setup([{ data: { logs: [], proxy_speed: { dry_run: false, compile_budget: '5s', async_warmup: true, warmup_user_min_hits: 2 } } }]);
    await context.loadLogs();
    const text = get('logsContainer').textContent;
    assert.doesNotMatch(text, /Product/);
    assert.match(text, /Claude Code/);
    assert.match(text, /Codex CLI/);
    assert.match(text, /Ollama/);
    assert.match(text, /Cursor \/ Cline \/ Cherry Studio/);
    assert.match(text, /127\.0\.0\.1:8080\/v1/);

    context.toggleQuickstartPanel(true);
    assert.equal(get('proxyQuickstartPanel').classList.contains('hidden'), false);
    assert.match(get('proxyQuickstartPanel').textContent, /ANTHROPIC_BASE_URL/);
});

test('dry-run proxy logs render banner, ROI chips, policy updates, and redacted diagnostics', async () => {
    const logItem = {
        id: 'dry-42',
        timestamp: '10:30:00',
        protocol: 'openai',
        role: 'system',
        decision: 'dry_run_would_adopt',
        reason: 'Dry-Run 观察模式：本次审计通过，且估算 Token更少，实际已原样转发原文',
        original: 'System prompt with sk-ant-secret1234567890abcdef token',
        compiled: '∀req: compressed with Bearer abcdef1234567890',
        ratio: '文本 Token（估算）100→60 (减少40.0%)',
        validation_method: 'cached',
        cache_hit: true,
        redacted: true
    };
    const payload = {
        proxy_speed: {
            dry_run: true,
            skip_cache_control: true,
            compress_developer: false,
            async_warmup: true,
            compile_budget: '10s',
            warmup_user_min_hits: 3
        },
        metrics: {
            requests_total: 4,
            adopted_total: 1,
            adopted_orig_tokens_total: 100,
            adopted_saved_tokens_total: 40,
            dry_run_total: 2,
            dry_run_would_adopt_total: 1,
            dry_run_orig_tokens_total: 200,
            dry_run_potential_saved_tokens_total: 80,
            fast_path_hit_total: 2,
            validation_rejected_total: 1,
            passthrough_total: 3
        },
        logs: [logItem]
    };
    const { context, get, requests } = setup([
        { data: payload },
        { data: { status: 'ok', proxy_speed: { ...payload.proxy_speed, dry_run: false, compile_budget: '30s' } } }
    ]);

    await context.loadLogs();
    assert.equal(get('dryRunBanner').classList.contains('hidden'), false);
    assert.equal(get('policyDryRunCheck').checked, true);
    assert.equal(get('policySkipCacheCheck').checked, true);
    assert.equal(get('policyCompileBudgetSelect').value, '10s');
    assert.equal(get('policyWarmupMinHitsInput').value, '3');

    const metricsText = get('proxyMetricsBar').textContent;
    assert.match(metricsText, /实省 Token: 40 \(-40\.0%\)/);
    assert.match(metricsText, /Dry-Run 预计可省: 80 \(-40\.0%\) · 命中 1\/2/);
    assert.match(metricsText, /快路径命中 2/);
    assert.match(metricsText, /校验拦截 1/);

    const logsText = get('logsContainer').textContent;
    assert.match(logsText, /Dry-Run 可省（原样转发）/);
    assert.match(logsText, /Dry-Run 候选（实际已原样转发）/);
    assert.match(logsText, /校验方式：缓存校验/);

    let copiedReport = '';
    context.navigator.clipboard.writeText = async text => { copiedReport = text; };
    const diagBtn = findElements(get('logsContainer'), el => el.tagName === 'BUTTON' && el.textContent === '复制脱敏诊断')[0];
    assert.ok(diagBtn, 'missing diagnostic export button');
    await diagBtn.onclick();
    assert.match(copiedReport, /Decision: dry_run_would_adopt/);
    assert.match(copiedReport, /sk-\*\*\*\[REDACTED\]/);
    assert.match(copiedReport, /Bearer \*\*\*\[REDACTED\]/);
    assert.doesNotMatch(copiedReport, /secret1234567890abcdef|abcdef1234567890/);
    assert.match(copiedReport, /"dry_run":true/);

    await context.updateProxyPolicyField({ dry_run: false, compile_budget: '30s' });
    const patchReq = JSON.parse(requests[1].options.body);
    assert.deepEqual(patchReq, { action: 'update_policy', dry_run: false, compile_budget: '30s' });
    assert.equal(get('dryRunBanner').classList.contains('hidden'), true);
    assert.equal(get('policyDryRunCheck').checked, false);
    assert.equal(get('policyCompileBudgetSelect').value, '30s');
});

test('frontend stays free of pictograph emojis and exposes clean endpoint copy + status labels', async () => {
    assert.equal(/[\u{1F300}-\u{1FAFF}\u{2600}-\u{27BF}\u{23F0}-\u{23FF}]/u.test(html), false, 'index.html must not contain pictograph emojis');
    const { context, get } = setup();
    get('proxyEndpointOpenAI').textContent = 'http://127.0.0.1:8080/v1';
    let copiedEndpoint = '';
    context.navigator.clipboard.writeText = async text => { copiedEndpoint = text; };
    const copyBtn = { textContent: '复制' };
    await context.copyEndpoint('proxyEndpointOpenAI', copyBtn);
    assert.equal(copiedEndpoint, 'http://127.0.0.1:8080/v1');
    assert.equal(copyBtn.textContent, '已复制');

    context.renderCircuitStatus({ state: 'CLOSED', failures: 0, threshold: 3 });
    assert.equal(get('circuitStatusPill').textContent, '熔断器 CLOSED (0/3)');
    context.renderCircuitStatus({ state: 'OPEN', remaining_sec: 42 });
    assert.equal(get('circuitStatusPill').textContent, '熔断直通 OPEN (剩余 42s)');
});

test('model config saves audit_profile, disable_thinking, and reasoning_effort', async () => {
    const saved = configFixture({ audit_profile: 'reader', disable_thinking: true, reasoning_effort: 'none' });
    const { context, get, requests, storage } = setup([
        { data: configFixture({ audit_profile: 'reader', disable_thinking: false, reasoning_effort: 'low' }) },
        { data: { success: true } },
        { data: saved }
    ]);
    await context.loadInitConfig();
    assert.equal(get('cfgAuditProfile').value, 'reader');
    assert.equal(get('cfgDisableThinking').checked, false);
    assert.equal(get('cfgReasoningEffort').value, 'low');

    get('cfgDisableThinking').checked = true;
    get('cfgReasoningEffort').value = 'none';
    await context.saveProfileConfig();
    const saveBody = JSON.parse(requests[1].options.body);
    assert.equal(saveBody.audit_profile, 'reader');
    assert.equal(saveBody.disable_thinking, true);
    assert.equal(saveBody.reasoning_effort, 'none');

    const backup = JSON.parse(storage.get('babel_dsl_backup_v2'));
    assert.equal(backup.disable_thinking, true);
    assert.equal(backup.reasoning_effort, 'none');
    assert.equal(get('cfgDisableThinking').checked, true);
    assert.equal(get('cfgReasoningEffort').value, 'none');
});

test('runCompile consumes SSE stream events and accepts local_fast auto short-circuits in strict mode', async () => {
    const encoder = new TextEncoder();
    const sseChunks = [
        'event: stage\ndata: {"stage":"generating"}\n\n',
        'event: delta\ndata: {"chunk":"draft1","reset":false}\n\n',
        'event: delta\ndata: {"chunk":"∀req: check","reset":true}\n\n',
        'event: stage\ndata: {"stage":"validating"}\n\n',
        `event: result\ndata: ${JSON.stringify({
            ...result({ compiled: '∀req: check', strategy: 'llm_parallel', elapsed_ms: 420 })
        })}\n\n`
    ];
    let idx = 0;
    const sseResponse = {
        status: 200,
        headers: { get: name => name.toLowerCase() === 'content-type' ? 'text/event-stream; charset=utf-8' : null },
        body: {
            getReader() {
                return {
                    async read() {
                        if (idx >= sseChunks.length) return { done: true, value: undefined };
                        return { done: false, value: encoder.encode(sseChunks[idx++]) };
                    }
                };
            }
        }
    };
    const fastShortCircuit = result({
        compiled: '短文本原样',
        strategy: 'passthrough',
        reason: '短文本低于 LLM 压缩收益阈值（<180 字或 <80 token），原样直通（<1ms）',
        elapsed_ms: 0,
        validation: validation('passed', 'local_fast')
    });
    const { context, get, requests } = setup([sseResponse, { data: fastShortCircuit }]);
    get('verifyCheck').checked = true;
    get('compileStrategy').value = 'auto';

    await context.runCompile();
    assert.equal(requests[0].url, '/api/compile/stream');
    assert.equal(get('dslOutput').value, '∀req: check');
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'passed');
    assert.match(get('statsBadge').textContent, /并行编译 · 420ms/);

    await context.runCompile();
    assert.equal(get('dslOutput').value, '短文本原样');
    assert.equal(get('invariantCapsules').dataset.validationStatus, 'passed');
    assert.equal(get('copyBtn').textContent, '复制结果');
    assert.match(get('statsBadge').textContent, /短文本直通 · 0ms/);
    assert.match(get('auditBox').textContent, /短文本低于 LLM 压缩收益阈值/);
});

test('proxy renders local_fast decision badge and updates local_fast_path policy', async () => {
    const { context, get, requests } = setup([
        {
            data: {
                proxy_speed: {
                    dry_run: false, skip_cache_control: false, compress_developer: false,
                    async_warmup: true, local_fast_path: true, compile_budget: '5s', warmup_user_min_hits: 2
                },
                metrics: { requests_total: 1, adopted_total: 1, adopted_orig_tokens_total: 120, adopted_saved_tokens_total: 30 },
                logs: [{
                    id: 'lf-1', timestamp: '14:30:00', protocol: 'openai', dialect: 'mixed', role: 'user',
                    decision: 'local_fast', reason: '本地无损压缩（<1ms）即时生效',
                    validation_method: 'local_fast', original: 'raw   text', compiled: 'raw text',
                    ratio: '文本 Token（估算）120→90 (减少25.0%)', measurement_kind: 'estimated'
                }]
            }
        },
        {
            data: {
                status: 'ok',
                proxy_speed: {
                    dry_run: false, skip_cache_control: false, compress_developer: false,
                    async_warmup: true, local_fast_path: false, compile_budget: '5s', warmup_user_min_hits: 2
                }
            }
        }
    ]);
    await context.loadLogs();
    assert.equal(get('policyLocalFastCheck').checked, true);
    assert.match(get('proxyMetricsBar').textContent, /本地快路径\(<1ms\)/);
    assert.match(get('logsContainer').textContent, /本地无损压缩/);
    assert.match(get('logsContainer').textContent, /实际发送：本地无损压缩/);
    assert.match(get('logsContainer').textContent, /校验方式：本地快路径/);

    await context.updateProxyPolicyField({ local_fast_path: false });
    assert.deepEqual(JSON.parse(requests[1].options.body), { action: 'update_policy', local_fast_path: false });
    assert.equal(get('policyLocalFastCheck').checked, false);
});

test('cancelCompile aborts in-flight stream request and restores editor controls cleanly', async () => {
    const { context, get, requests } = setup([
        options => new Promise((_, reject) => {
            assert.ok(options.signal, 'expected AbortSignal on compile request');
            options.signal.addEventListener('abort', () => {
                const err = new Error('Aborted');
                err.name = 'AbortError';
                reject(err);
            });
        })
    ]);
    get('compileStrategy').value = 'deep';
    const pending = context.runCompile();
    assert.equal(get('runBtn').disabled, true);
    assert.equal(get('stopBtn').classList.contains('hidden'), false);
    assert.equal(get('compileProgressBar').classList.contains('hidden'), false);

    context.cancelCompile();
    await pending;

    assert.equal(requests[0].options.signal.aborted, true);
    assert.equal(get('runBtn').disabled, false);
    assert.equal(get('runBtn').textContent, '编译');
    assert.equal(get('forceBtn').disabled, false);
    assert.equal(get('stopBtn').classList.contains('hidden'), true);
    assert.equal(get('compileProgressBar').classList.contains('hidden'), true);
    assert.match(get('auditBox').textContent, /已手动停止本次编译/);
});

test('route preview and multi-scenario demo presets reflect short, JSON, and parallel lengths', () => {
    const { context, get } = setup();
    get('compileStrategy').value = 'auto';

    context.fillDemoText('short');
    assert.match(get('inputCharCount').textContent, /字 · ~\d+ tok/);
    assert.match(get('routePreviewBadge').textContent, /短文本直通\/本地/);
    assert.equal(get('routePreviewBadge').classList.contains('is-fast'), true);

    context.fillDemoText('json');
    assert.match(get('routePreviewBadge').textContent, /JSON\/代码本地快路径/);
    assert.equal(get('routePreviewBadge').classList.contains('is-fast'), true);

    context.fillDemoText('agent');
    assert.match(get('routePreviewBadge').textContent, /单次语义编译/);
    assert.equal(get('routePreviewBadge').classList.contains('is-llm'), true);

    context.fillDemoText('sop');
    assert.match(get('routePreviewBadge').textContent, /并行分块编译/);
    assert.equal(get('routePreviewBadge').classList.contains('is-parallel'), true);

    context.toggleOperatorBar();
    assert.equal(get('operatorBar').classList.contains('hidden'), false);
    context.clearCompileInput();
    assert.equal(get('rawInput').value, '');
    assert.equal(get('routePreviewBadge').classList.contains('hidden'), true);
});

test('compile history saves bounded results without secrets and forceDeepBtn overrides auto passthrough', async () => {
    const shortResp = result({
        compiled: '短文本原样',
        strategy: 'passthrough',
        reason: '短文本低于阈值',
        elapsed_ms: 1,
        validation: validation('passed', 'local_fast')
    });
    const deepResp = result({
        compiled: '∀order: ¬delete',
        strategy: 'llm_compile',
        elapsed_ms: 350,
        validation: validation('passed', 'llm_compare')
    });
    const { context, get, requests, storage } = setup([
        { data: shortResp },
        { data: deepResp }
    ]);
    get('verifyCheck').checked = true;
    get('compileStrategy').value = 'auto';
    await context.runCompile();

    assert.equal(get('forceDeepBtn').classList.contains('hidden'), false);
    assert.match(get('historyToggleBtn').textContent, /历史 \(1\)/);

    const savedHist = JSON.parse(storage.get('babel_compile_history_v1'));
    assert.equal(savedHist.length, 1);
    assert.equal(Object.hasOwn(savedHist[0], 'api_key'), false);

    await context.forceDeepCompile();
    assert.equal(get('compileStrategy').value, 'deep');
    assert.equal(JSON.parse(requests[1].options.body).strategy, 'deep');
    assert.equal(JSON.parse(requests[1].options.body).no_cache, true);
    assert.equal(get('forceDeepBtn').classList.contains('hidden'), true);
    assert.equal(get('dslOutput').value, '∀order: ¬delete');

    context.toggleHistoryPanel(true);
    assert.equal(get('historyPanel').classList.contains('hidden'), false);
    context.restoreCompileHistory(1);
    assert.equal(get('dslOutput').value, '短文本原样');
    assert.equal(get('historyPanel').classList.contains('hidden'), true);
});

test('proxy log filters and dry-run self-test probe work cleanly', async () => {
    const logsPayload = {
        proxy_speed: { dry_run: false, async_warmup: true, local_fast_path: true, compile_budget: '5s', warmup_user_min_hits: 2 },
        metrics: { requests_total: 3 },
        logs: [
            { id: '1', timestamp: '10:00:01', protocol: 'openai', role: 'system', decision: 'adopted', reason: 'system compressed', original: 'sys orig', compiled: 'sys comp' },
            { id: '2', timestamp: '10:00:02', protocol: 'openai', role: 'user', decision: 'local_fast', reason: 'whitespace stripped', original: 'user   raw', compiled: 'user raw' },
            { id: '3', timestamp: '10:00:03', protocol: 'openai', role: 'tool:search', decision: 'kept_original', reason: 'short tool', original: 'tool orig', compiled: 'tool orig' }
        ]
    };
    const { context, get, requests } = setup([
        { data: logsPayload },
        { status: 200, headers: { get: name => name === 'X-Babel-Status' ? 'dry-run-hit' : '5' }, data: {} },
        { data: logsPayload }
    ]);
    await context.loadLogs();
    assert.equal(get('proxyFilterCount').textContent, '共 3 条');

    context.setProxyLogFilter('local_fast');
    assert.equal(get('proxyFilterCount').textContent, '显示 1 / 3 条');
    assert.match(get('logsContainer').textContent, /whitespace stripped/);
    assert.doesNotMatch(get('logsContainer').textContent, /system compressed/);

    context.setProxyLogFilter('system_tools');
    assert.equal(get('proxyFilterCount').textContent, '显示 2 / 3 条');

    get('proxyLogSearchInput').value = 'tool:search';
    context.renderFilteredProxyLogs();
    assert.equal(get('proxyFilterCount').textContent, '显示 1 / 3 条');

    get('proxyLogSearchInput').value = '';
    context.setProxyLogFilter('all');
    await context.runProxyDryRunProbe();
    assert.equal(requests[1].url, '/v1/chat/completions');
    assert.equal(requests[1].options.headers['X-Babel-Dry-Run'], '1');
    assert.match(get('policySaveHint').textContent, /Dry-Run/);
});
