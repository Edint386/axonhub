/** Slim copy of lm-detector `shared/types.ts`, limited to scoring types used in the browser. */

export interface Challenge {
  id: string
  expected_count: number
  prompt: string
}

export interface BankModel {
  id: string
  display_name: string
  family: string
  family_name: string
  response_count: number
  valid_number_count?: number
  counts?: number[]
  nearest_models?: { model: string; distance: number }[]
  added_at?: string
}

export interface Bank {
  schema?: string
  built_at: string
  models: BankModel[]
  detector_schema?: string
  [key: string]: unknown
}

export interface Output {
  text: string
  expected_count: number
}

export interface Analysis {
  prediction: string
  prediction_name: string
  probability: number | null
  evidence: { insufficient: boolean; label: string; reason: string; threshold: number | null; method: string }
  used_outputs: number
  results: { model: string; display_name: string; family: string; family_name: string; probability: number | null; score: number }[]
  diagnostics: { index: number; parsed_numbers: number; minimum_numbers: number; accepted: boolean }[]
  family_prediction?: string
  family_prediction_name: string
  ranking_score?: number
  decision?: string
  sequence?: { autocorrelation: number; small_step_share: number; model_like: boolean } | null
  [key: string]: unknown
}
