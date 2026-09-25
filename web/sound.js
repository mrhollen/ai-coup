// ===========================================================================
// sound.js — Zero-dependency Web Audio procedural sound synthesizer for Coup.
// Procedural audio: coin clinks, card slides, challenge strikes, turn chimes,
// and UI tap feedback using native AudioContext oscillators & filters.
// ===========================================================================

const SOUND_STORAGE_KEY = "coup.sound";

let audioCtx = null;
let masterGain = null;

/**
 * Returns or creates the singleton AudioContext and master gain node.
 * Safe to call in browser environments; returns null in headless/SSR.
 */
export function getAudioContext() {
  if (typeof window === "undefined") return null;
  if (!audioCtx) {
    try {
      const AudioCtxClass = window.AudioContext || window.webkitAudioContext;
      if (!AudioCtxClass) return null;
      audioCtx = new AudioCtxClass();
      masterGain = audioCtx.createGain();
      masterGain.gain.setValueAtTime(0.35, audioCtx.currentTime);
      masterGain.connect(audioCtx.destination);
    } catch {
      return null;
    }
  }
  return audioCtx;
}

/**
 * Initializes or resumes the AudioContext on user interaction to satisfy
 * browser autoplay policies.
 */
export function initAudio() {
  const ctx = getAudioContext();
  if (ctx && ctx.state === "suspended") {
    ctx.resume().catch(() => {});
  }
  return ctx;
}

/**
 * Checks if sound effects are enabled. Defaults to true (unmuted).
 */
export function isSoundEnabled() {
  try {
    const val = localStorage.getItem(SOUND_STORAGE_KEY);
    if (val === null) return true;
    return val === "1" || val === "true";
  } catch {
    return true;
  }
}

/**
 * Toggles sound enabled state and saves to localStorage.
 * Returns the new state (true = enabled, false = muted).
 */
export function toggleSound() {
  const next = !isSoundEnabled();
  try {
    localStorage.setItem(SOUND_STORAGE_KEY, next ? "1" : "0");
  } catch {}
  if (next) {
    initAudio();
    playClick();
  }
  window.dispatchEvent(new CustomEvent("coup:sound-changed", { detail: { enabled: next } }));
  return next;
}

/**
 * Crisp metallic coin clink sound.
 * Uses high-frequency resonant sine oscillators with sharp attacks and fast decays,
 * paired with a secondary delayed clink to simulate coins colliding.
 */
export function playCoin() {
  if (!isSoundEnabled()) return;
  try {
    const ctx = initAudio();
    if (!ctx || ctx.state !== "running" || !masterGain) return;

    const now = ctx.currentTime;
    const clinks = [
      { time: now, freq1: 2400, freq2: 4850, gain: 0.22, decay: 0.12 },
      { time: now + 0.038, freq1: 2750, freq2: 5400, gain: 0.18, decay: 0.15 },
    ];

    clinks.forEach(({ time, freq1, freq2, gain, decay }) => {
      // Primary resonant pitch
      const osc1 = ctx.createOscillator();
      const g1 = ctx.createGain();
      osc1.type = "sine";
      osc1.frequency.setValueAtTime(freq1, time);
      g1.gain.setValueAtTime(gain, time);
      g1.gain.exponentialRampToValueAtTime(0.0001, time + decay);
      osc1.connect(g1);
      g1.connect(masterGain);
      osc1.start(time);
      osc1.stop(time + decay);

      // High inharmonic metallic overtone
      const osc2 = ctx.createOscillator();
      const g2 = ctx.createGain();
      osc2.type = "sine";
      osc2.frequency.setValueAtTime(freq2, time);
      g2.gain.setValueAtTime(gain * 0.4, time);
      g2.gain.exponentialRampToValueAtTime(0.0001, time + decay * 0.7);
      osc2.connect(g2);
      g2.connect(masterGain);
      osc2.start(time);
      osc2.stop(time + decay * 0.7);
    });
  } catch {
    // Non-critical audio failure; fail gracefully
  }
}

/**
 * Soft card slide / flip sound.
 * Procedural bandpass-filtered noise burst with swept center frequency
 * followed by a soft tactile landing tap.
 */
export function playCard() {
  if (!isSoundEnabled()) return;
  try {
    const ctx = initAudio();
    if (!ctx || ctx.state !== "running" || !masterGain) return;

    const now = ctx.currentTime;
    const duration = 0.13;

    // Synthesize white noise buffer
    const bufferSize = Math.floor(ctx.sampleRate * duration);
    const buffer = ctx.createBuffer(1, bufferSize, ctx.sampleRate);
    const data = buffer.getChannelData(0);
    for (let i = 0; i < bufferSize; i++) {
      data[i] = Math.random() * 2 - 1;
    }

    const noiseSource = ctx.createBufferSource();
    noiseSource.buffer = buffer;

    // Dynamic bandpass filter sweeps downward to simulate card friction
    const filter = ctx.createBiquadFilter();
    filter.type = "bandpass";
    filter.frequency.setValueAtTime(1500, now);
    filter.frequency.exponentialRampToValueAtTime(650, now + duration);
    filter.Q.setValueAtTime(1.6, now);

    const gain = ctx.createGain();
    gain.gain.setValueAtTime(0.001, now);
    gain.gain.linearRampToValueAtTime(0.16, now + 0.02);
    gain.gain.exponentialRampToValueAtTime(0.0001, now + duration);

    noiseSource.connect(filter);
    filter.connect(gain);
    gain.connect(masterGain);

    noiseSource.start(now);
    noiseSource.stop(now + duration);

    // Subtle soft tap at end of slide
    const tap = ctx.createOscillator();
    const tapGain = ctx.createGain();
    tap.type = "triangle";
    tap.frequency.setValueAtTime(240, now + 0.05);
    tap.frequency.exponentialRampToValueAtTime(70, now + 0.11);
    tapGain.gain.setValueAtTime(0.07, now + 0.05);
    tapGain.gain.exponentialRampToValueAtTime(0.0001, now + 0.11);
    tap.connect(tapGain);
    tapGain.connect(masterGain);
    tap.start(now + 0.05);
    tap.stop(now + 0.11);
  } catch {
    // Non-critical audio failure
  }
}

/**
 * Dramatic low resonant thud / strike sound on challenge or coup.
 * Deep pitch-dropped gavel strike with wooden table resonance.
 */
export function playChallenge() {
  if (!isSoundEnabled()) return;
  try {
    const ctx = initAudio();
    if (!ctx || ctx.state !== "running" || !masterGain) return;

    const now = ctx.currentTime;

    // 1. Pitch-dropping strike impact (gavel hitting table)
    const osc1 = ctx.createOscillator();
    const g1 = ctx.createGain();
    osc1.type = "sine";
    osc1.frequency.setValueAtTime(170, now);
    osc1.frequency.exponentialRampToValueAtTime(40, now + 0.28);
    g1.gain.setValueAtTime(0.40, now);
    g1.gain.exponentialRampToValueAtTime(0.0001, now + 0.35);
    osc1.connect(g1);
    g1.connect(masterGain);
    osc1.start(now);
    osc1.stop(now + 0.35);

    // 2. Sub-bass wood resonance
    const osc2 = ctx.createOscillator();
    const g2 = ctx.createGain();
    osc2.type = "triangle";
    osc2.frequency.setValueAtTime(54, now);
    osc2.frequency.exponentialRampToValueAtTime(36, now + 0.45);
    g2.gain.setValueAtTime(0.26, now);
    g2.gain.exponentialRampToValueAtTime(0.0001, now + 0.45);
    osc2.connect(g2);
    g2.connect(masterGain);
    osc2.start(now);
    osc2.stop(now + 0.45);

    // 3. Tactile initial click/thump
    const noiseSize = Math.floor(ctx.sampleRate * 0.035);
    const noiseBuf = ctx.createBuffer(1, noiseSize, ctx.sampleRate);
    const d = noiseBuf.getChannelData(0);
    for (let i = 0; i < noiseSize; i++) d[i] = Math.random() * 2 - 1;
    const noiseSrc = ctx.createBufferSource();
    noiseSrc.buffer = noiseBuf;

    const noiseFilter = ctx.createBiquadFilter();
    noiseFilter.type = "lowpass";
    noiseFilter.frequency.setValueAtTime(300, now);

    const noiseGain = ctx.createGain();
    noiseGain.gain.setValueAtTime(0.28, now);
    noiseGain.gain.exponentialRampToValueAtTime(0.001, now + 0.035);

    noiseSrc.connect(noiseFilter);
    noiseFilter.connect(noiseGain);
    noiseGain.connect(masterGain);
    noiseSrc.start(now);
    noiseSrc.stop(now + 0.035);
  } catch {
    // Non-critical audio failure
  }
}

/**
 * Gentle chime alerting the player that it is their turn.
 * Dual-tone crystal parlor chime (E5 and A5) with gentle harmonic decay.
 */
export function playTurn() {
  if (!isSoundEnabled()) return;
  try {
    const ctx = initAudio();
    if (!ctx || ctx.state !== "running" || !masterGain) return;

    const now = ctx.currentTime;
    const notes = [
      { time: now, freq: 659.25, gain: 0.16, decay: 0.65 },        // E5
      { time: now + 0.09, freq: 880.0, gain: 0.20, decay: 0.85 },    // A5
      { time: now + 0.09, freq: 1760.0, gain: 0.08, decay: 0.45 },   // A6 shimmer
    ];

    notes.forEach(({ time, freq, gain, decay }) => {
      const osc = ctx.createOscillator();
      const g = ctx.createGain();
      osc.type = "sine";
      osc.frequency.setValueAtTime(freq, time);
      g.gain.setValueAtTime(0.001, time);
      g.gain.linearRampToValueAtTime(gain, time + 0.012);
      g.gain.exponentialRampToValueAtTime(0.0001, time + decay);
      osc.connect(g);
      g.connect(masterGain);
      osc.start(time);
      osc.stop(time + decay);
    });
  } catch {
    // Non-critical audio failure
  }
}

/**
 * Subtle UI tap feedback for button clicks.
 */
export function playClick() {
  if (!isSoundEnabled()) return;
  try {
    const ctx = initAudio();
    if (!ctx || ctx.state !== "running" || !masterGain) return;

    const now = ctx.currentTime;
    const osc = ctx.createOscillator();
    const g = ctx.createGain();
    osc.type = "sine";
    osc.frequency.setValueAtTime(800, now);
    osc.frequency.exponentialRampToValueAtTime(250, now + 0.018);
    g.gain.setValueAtTime(0.05, now);
    g.gain.exponentialRampToValueAtTime(0.0001, now + 0.018);
    osc.connect(g);
    g.connect(masterGain);
    osc.start(now);
    osc.stop(now + 0.018);
  } catch {
    // Non-critical audio failure
  }
}

/**
 * Urgent double-tone warning chord when an AI or opponent targets the human player.
 */
export function playTargetAlert() {
  if (!isSoundEnabled()) return;
  try {
    const ctx = initAudio();
    if (!ctx || ctx.state !== "running" || !masterGain) return;

    const now = ctx.currentTime;
    const tones = [
      { time: now, freq: 587.33, decay: 0.16, gain: 0.22 },
      { time: now, freq: 622.25, decay: 0.16, gain: 0.18 },
      { time: now + 0.14, freq: 587.33, decay: 0.28, gain: 0.26 },
      { time: now + 0.14, freq: 783.99, decay: 0.32, gain: 0.22 },
    ];

    tones.forEach(({ time, freq, decay, gain }) => {
      const osc = ctx.createOscillator();
      const g = ctx.createGain();
      osc.type = "sawtooth";
      const filter = ctx.createBiquadFilter();
      filter.type = "lowpass";
      filter.frequency.setValueAtTime(1800, time);

      osc.frequency.setValueAtTime(freq, time);
      g.gain.setValueAtTime(0.001, time);
      g.gain.linearRampToValueAtTime(gain, time + 0.015);
      g.gain.exponentialRampToValueAtTime(0.0001, time + decay);

      osc.connect(filter);
      filter.connect(g);
      g.connect(masterGain);
      osc.start(time);
      osc.stop(time + decay);
    });
  } catch {
    // Non-critical audio failure
  }
}

/**
 * Noticeable yet polite cue when an opponent declares an action.
 */
export function playActionAlert() {
  if (!isSoundEnabled()) return;
  try {
    const ctx = initAudio();
    if (!ctx || ctx.state !== "running" || !masterGain) return;

    const now = ctx.currentTime;
    const osc = ctx.createOscillator();
    const g = ctx.createGain();
    osc.type = "triangle";
    osc.frequency.setValueAtTime(440, now);
    osc.frequency.exponentialRampToValueAtTime(554.37, now + 0.08);
    g.gain.setValueAtTime(0.001, now);
    g.gain.linearRampToValueAtTime(0.12, now + 0.01);
    g.gain.exponentialRampToValueAtTime(0.0001, now + 0.16);

    osc.connect(g);
    g.connect(masterGain);
    osc.start(now);
    osc.stop(now + 0.16);
  } catch {
    // Non-critical audio failure
  }
}

