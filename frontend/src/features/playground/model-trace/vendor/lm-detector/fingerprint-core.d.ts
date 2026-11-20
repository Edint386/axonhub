export const VALUE_MIN: number
export const VALUE_MAX: number
export const DIMENSION: number
export const ALPHA: number
export function parseNumbers(text: string): number[]
export function countNumbers(numbers: number[]): number[]
export function hellingerFeature(counts: number[]): number[]
export function orderedBlockFeature(numbers: number[]): number[]
export function robustScoreNumbers(numbers: number[], bank: unknown): number[]
