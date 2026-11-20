// Official multilingual challenge templates from lm-detector (shared/challenge-browser.js).
// The vendored copy avoids crypto.randomUUID, which browsers only expose on HTTPS or localhost.

export type { Challenge } from './vendor/lm-detector/types.ts';
export { generateChallenges } from './vendor/lm-detector/challenge-browser.js';
