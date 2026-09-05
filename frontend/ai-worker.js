// Offline AI in a Web Worker — keeps model inference off the main thread so the
// UI stays responsive while tokens generate.
//
// Messages IN:  { type: 'load', model }   { type: 'generate', system, user, id }   { type: 'stop' }
// Messages OUT: { type: 'progress', pct, loaded, total, file }
//               { type: 'ready', model }  { type: 'done', id, text }  { type: 'error', message }

const CDN = 'https://cdn.jsdelivr.net/npm/@xenova/transformers@2.17.2';

const MODELS = {
  'qwen-0.5b':      { id: 'Xenova/Qwen1.5-0.5B-Chat',        template: 'chatml' },
  'tinyllama-1.1b': { id: 'Xenova/TinyLlama-1.1B-Chat-v1.0', template: 'zephyr' },
  'phi3-mini':      { id: 'Xenova/Phi-3-mini-4k-instruct',   template: 'phi3'   },
};

let tf = null;
let pipe = null;
let pipeKey = null;
let cancelled = false;

async function transformers() {
  if (tf) return tf;
  tf = await import(CDN);
  tf.env.allowLocalModels = false;
  tf.env.useBrowserCache = true;
  return tf;
}

function buildPrompt(template, system, user) {
  if (template === 'chatml') {
    return '<|im_start|>system\n' + system + '<|im_end|>\n' +
           '<|im_start|>user\n' + user + '<|im_end|>\n<|im_start|>assistant\n';
  }
  if (template === 'phi3') {
    return '<|system|>\n' + system + '<|end|>\n<|user|>\n' + user + '<|end|>\n<|assistant|>\n';
  }
  return '<|system|>\n' + system + '</s>\n<|user|>\n' + user + '</s>\n<|assistant|>\n';
}

const STOPS = ['<|im_end|>', '<|end|>', '</s>', '<|user|>', '<|im_start|>', '<|endoftext|>'];

async function load(key) {
  const model = MODELS[key] || MODELS['qwen-0.5b'];
  if (pipe && pipeKey === key) { self.postMessage({ type: 'ready', model: key }); return; }
  const lib = await transformers();
  const files = {};
  pipe = await lib.pipeline('text-generation', model.id, {
    quantized: true,
    progress_callback: (p) => {
      if (p && p.file && typeof p.total === 'number' && p.total > 0) {
        files[p.file] = { loaded: p.loaded || 0, total: p.total };
      }
      let loaded = 0, total = 0;
      Object.keys(files).forEach((f) => { loaded += files[f].loaded; total += files[f].total; });
      self.postMessage({
        type: 'progress',
        file: p && p.file,
        loaded, total,
        pct: total ? Math.min(100, Math.round((loaded / total) * 100)) : 0,
      });
    },
  });
  pipeKey = key;
  self.postMessage({ type: 'ready', model: key });
}

async function generate(key, system, user, id) {
  cancelled = false;
  const model = MODELS[key] || MODELS['qwen-0.5b'];
  if (!pipe || pipeKey !== key) await load(key);
  const prompt = buildPrompt(model.template, system, user);
  const out = await pipe(prompt, {
    max_new_tokens: 220,
    temperature: 0.3,
    do_sample: false,
    repetition_penalty: 1.12,
    return_full_text: false,
    callback_function: () => { if (cancelled) throw new Error('cancelled'); },
  });
  let text = Array.isArray(out) ? (out[0] && out[0].generated_text) || '' : (out && out.generated_text) || '';
  text = String(text || '');
  const tail = prompt.slice(-40);
  const at = text.indexOf(tail);
  if (at >= 0) text = text.slice(at + tail.length);
  STOPS.forEach((s) => { const i = text.indexOf(s); if (i >= 0) text = text.slice(0, i); });
  self.postMessage({ type: 'done', id, text: text.trim() });
}

self.onmessage = async (e) => {
  const m = e.data || {};
  try {
    if (m.type === 'load') await load(m.model);
    else if (m.type === 'generate') await generate(m.model, m.system, m.user, m.id);
    else if (m.type === 'stop') cancelled = true;
  } catch (err) {
    if (String(err && err.message) !== 'cancelled') {
      self.postMessage({ type: 'error', message: String((err && err.message) || err) });
    }
  }
};
