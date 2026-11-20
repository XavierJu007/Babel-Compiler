const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
// Exercise the shipped animation and keyboard listeners, but omit startup API
// calls. Timers are captured without running the proxy's polling callback.
const startup = script.lastIndexOf('    loadDomainPacks().catch(');
assert.ok(startup > 0, 'startup API boundary must exist');

function eventTarget(extra = {}) {
    const listeners = new Map();
    return Object.assign({
        addEventListener(type, listener) {
            if (!listeners.has(type)) listeners.set(type, []);
            listeners.get(type).push(listener);
        },
        emit(type, event = {}) {
            for (const listener of listeners.get(type) || []) listener(event);
        }
    }, extra);
}

function setup({ reduced = false, hidden = false } = {}) {
    const document = eventTarget({ hidden, activeElement: null });
    class Element {
        constructor(id = '', classes = '') {
            this.id = id;
            this.attributes = new Map();
            this.classes = new Set(classes.split(/\s+/).filter(Boolean));
            this.controls = [];
            this.inert = false;
            this.disabled = false;
            this.visible = true;
            this.classList = {
                contains: name => this.classes.has(name),
                toggle: (name, force = !this.classes.has(name)) => {
                    force ? this.classes.add(name) : this.classes.delete(name);
                    return force;
                }
            };
        }
        setAttribute(name, value) { this.attributes.set(name, String(value)); }
        getAttribute(name) { return this.attributes.get(name) ?? null; }
        focus() { if (!this.disabled && !this.inert) document.activeElement = this; }
        contains(element) { return element === this || this.controls.includes(element); }
        getClientRects() { return this.visible ? [{}] : []; }
        querySelectorAll(selector) {
            return this.controls.filter(element => !selector.includes(':not(:disabled)') || !element.disabled);
        }
    }
    const elements = new Map();
    for (const match of html.matchAll(/<[^>]+\bid="([^"]+)"[^>]*>/g)) {
        const classes = match[0].match(/\bclass="([^"]*)"/)?.[1] || '';
        elements.set(match[1], new Element(match[1], classes));
    }
    const get = id => {
        assert.ok(elements.has(id), `missing shipped DOM element: ${id}`);
        return elements.get(id);
    };
    document.getElementById = get;
    document.body = new Element('body');
    const media = eventTarget({ matches: reduced });
    const window = eventTarget({
        innerWidth: 1440, innerHeight: 900, devicePixelRatio: 1,
        location: { origin: 'http://127.0.0.1:8080' },
        matchMedia: () => media
    });
    const drawing = { draws: 0 };
    const ctx = {
        clearRect() { drawing.draws++; },
        setTransform() {}, beginPath() {}, moveTo() {}, lineTo() {},
        stroke() {}, arc() {}, fill() {}
    };
    get('bg-canvas').getContext = () => ctx;
    const frames = new Map();
    let nextFrame = 0;
    const context = vm.createContext({
        document, window,
        requestAnimationFrame: callback => { frames.set(++nextFrame, callback); return nextFrame; },
        cancelAnimationFrame: id => frames.delete(id),
        setInterval: () => 1,
        fetch: () => { throw new Error('Theme regression must not call the network'); }
    });
    vm.runInContext(script.slice(0, startup), context, { filename: 'index.html' });
    return {
        context, document, window, get, media, frames, drawing,
        element: () => new Element(),
        tick(timestamp) {
            for (const [id, callback] of [...frames]) {
                frames.delete(id);
                callback(timestamp);
            }
        },
        key(key, shiftKey = false) {
            const event = { key, shiftKey, prevented: false,
                preventDefault() { this.prevented = true; } };
            document.emit('keydown', event);
            return event;
        }
    };
}

test('reduced motion draws a static background and resumes only one animation chain', () => {
    const ui = setup({ reduced: true });
    assert.equal(ui.drawing.draws, 1);
    assert.equal(ui.frames.size, 0);
    ui.window.emit('resize');
    assert.equal(ui.drawing.draws, 2);
    assert.equal(ui.frames.size, 0);

    ui.media.matches = false;
    ui.media.emit('change');
    assert.equal(ui.frames.size, 1);
    for (let frame = 1; frame <= 4; frame++) {
        ui.tick(frame * 16.67);
        assert.equal(ui.frames.size, 1);
    }
    ui.media.matches = true;
    ui.media.emit('change');
    assert.equal(ui.frames.size, 0);
    const stoppedAt = ui.drawing.draws;
    ui.tick(200);
    assert.equal(ui.drawing.draws, stoppedAt);
});

test('hidden pages stop drawing; repeated visibility and resize events never multiply RAF chains', () => {
    const ui = setup();
    assert.equal(ui.frames.size, 1);
    ui.tick(16.67);
    ui.document.hidden = true;
    ui.document.emit('visibilitychange');
    const stoppedAt = ui.drawing.draws;
    ui.window.emit('resize');
    ui.document.emit('visibilitychange');
    ui.media.emit('change');
    ui.tick(1000);
    assert.equal(ui.frames.size, 0);
    assert.equal(ui.drawing.draws, stoppedAt);

    ui.document.hidden = false;
    for (let event = 0; event < 5; event++) {
        ui.document.emit('visibilitychange');
        ui.window.emit('resize');
        assert.equal(ui.frames.size, 1);
    }
    const resumedAt = ui.drawing.draws;
    ui.tick(1017);
    assert.equal(ui.drawing.draws, resumedAt + 1);
    assert.equal(ui.frames.size, 1);
    const initiallyHidden = setup({ hidden: true });
    assert.equal(initiallyHidden.drawing.draws, 0);
    assert.equal(initiallyHidden.frames.size, 0);
});

test('Zen makes controls inert and Escape restores the current view and toggle focus', () => {
    const ui = setup({ reduced: true });
    const toggle = ui.get('zenToggleBtn');
    toggle.focus();
    ui.context.toggleZenMode();
    assert.equal(ui.document.body.classList.contains('zen-mode'), true);
    assert.equal(toggle.getAttribute('aria-pressed'), 'true');
    for (const id of ['mainNav', 'headerControls', 'viewCompiler', 'viewProxy']) {
        assert.equal(ui.get(id).inert, true, id);
        assert.equal(ui.get(id).classList.contains('ui-hidden'), true, id);
    }
    assert.equal(toggle.inert, false);
    ui.key('Escape');
    assert.equal(ui.document.body.classList.contains('zen-mode'), false);
    assert.equal(toggle.getAttribute('aria-pressed'), 'false');
    assert.equal(ui.document.activeElement, toggle);
    for (const id of ['mainNav', 'headerControls', 'viewCompiler', 'viewProxy']) {
        assert.equal(ui.get(id).inert, false, id);
        assert.equal(ui.get(id).classList.contains('ui-hidden'), false, id);
    }
    assert.equal(ui.get('viewProxy').classList.contains('hidden'), true);
    assert.equal(ui.get('viewCompiler').classList.contains('hidden'), false);
});

test('modal keyboard navigation skips unavailable controls and restores its original trigger', () => {
    const ui = setup({ reduced: true });
    const trigger = ui.get('zenToggleBtn');
    const first = ui.element(), disabled = ui.element(), concealed = ui.element(), last = ui.element();
    disabled.disabled = true;
    concealed.visible = false;
    const modal = ui.get('configModal');
    modal.controls = [first, disabled, last, concealed];
    trigger.focus();
    ui.context.setModalVisible('configModal', true);
    assert.equal(ui.document.activeElement, first);
    assert.equal(modal.classList.contains('hidden'), false);
    ui.context.setModalVisible('configModal', true);
    last.focus();
    assert.equal(ui.key('Tab').prevented, true);
    assert.equal(ui.document.activeElement, first);
    assert.equal(ui.key('Tab', true).prevented, true);
    assert.equal(ui.document.activeElement, last);
    trigger.focus();
    assert.equal(ui.key('Tab').prevented, true);
    assert.equal(ui.document.activeElement, first);
    assert.equal(ui.key('Escape').prevented, true);
    assert.equal(modal.classList.contains('hidden'), true);
    assert.equal(ui.document.activeElement, trigger);
});
