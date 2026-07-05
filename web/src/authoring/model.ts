import type { components } from '../api/schema'

export type Quiz = components['schemas']['Quiz']
export type TeacherQuestion = components['schemas']['TeacherQuestion']
export type QuestionInput = components['schemas']['QuestionInput']
export type QuestionType = QuestionInput['type']
export type QuestionOption = components['schemas']['QuestionOption']
export type ApiError = components['schemas']['Error']

export const TYPE_LABEL: Record<QuestionType, string> = {
  single: 'Single choice',
  multi: 'Multi select',
  truefalse: 'True / false',
  short: 'Short answer',
}

export const STATUS_LABEL: Record<Quiz['status'], string> = {
  draft: 'Draft',
  scheduled: 'Scheduled',
  live: 'Live',
  closed: 'Closed',
  archived: 'Archived',
}

/**
 * The editor's working copy of one question. All four type-specific answer
 * shapes are kept side by side so switching inputs never loses work; toInput
 * serializes only the fields the current type owns.
 */
export interface QuestionDraft {
  type: QuestionType
  text: string
  options: QuestionOption[]
  correctKeys: string[]
  correctBool: boolean
  accepted: string[]
  points: number
}

const OPTION_KEYS = ['a', 'b', 'c', 'd', 'e', 'f', 'g', 'h']

export function nextOptionKey(options: QuestionOption[]): string | null {
  const used = new Set(options.map((o) => o.key))
  return OPTION_KEYS.find((k) => !used.has(k)) ?? null
}

export function fromQuestion(q: TeacherQuestion): QuestionDraft {
  const draft: QuestionDraft = {
    type: q.type,
    text: q.body.text,
    options: q.options ?? [],
    correctKeys: [],
    correctBool: true,
    accepted: [],
    points: q.points,
  }
  switch (q.type) {
    case 'single':
      if (typeof q.correct === 'string') draft.correctKeys = [q.correct]
      break
    case 'multi':
      if (Array.isArray(q.correct)) {
        draft.correctKeys = q.correct.filter(
          (k): k is string => typeof k === 'string',
        )
      }
      break
    case 'truefalse':
      draft.correctBool = q.correct === true
      break
    case 'short': {
      const accepted = (q.correct as { accepted?: unknown })?.accepted
      if (Array.isArray(accepted)) {
        draft.accepted = accepted.filter(
          (a): a is string => typeof a === 'string',
        )
      }
      break
    }
  }
  return draft
}

/**
 * Serialize a draft to the wire shape, mirroring the server's per-type rules
 * (docs/07) so obviously incomplete state fails fast with the same field
 * vocabulary instead of a round trip.
 */
export function toInput(
  draft: QuestionDraft,
): { input: QuestionInput } | { fields: Record<string, string> } {
  const fields: Record<string, string> = {}
  if (draft.text.trim() === '') fields.body = 'The question text is required.'
  if (!Number.isFinite(draft.points) || draft.points <= 0) {
    fields.points = 'Points must be greater than zero.'
  } else if (draft.points > 1000) {
    fields.points = 'Points must be at most 1000.'
  }

  const input: QuestionInput = {
    type: draft.type,
    body: { text: draft.text.trim() },
    correct: null,
    points: draft.points,
  }
  switch (draft.type) {
    case 'single':
    case 'multi': {
      if (draft.options.some((o) => o.text.trim() === '')) {
        fields.options = 'Every option needs a text.'
      }
      input.options = draft.options.map((o) => ({
        key: o.key,
        text: o.text.trim(),
      }))
      const keys = draft.correctKeys.filter((k) =>
        draft.options.some((o) => o.key === k),
      )
      if (keys.length === 0) {
        fields.correct =
          draft.type === 'single'
            ? 'Mark one option as the correct answer.'
            : 'Mark at least one option as correct.'
      }
      input.correct = draft.type === 'single' ? keys[0] : keys
      break
    }
    case 'truefalse':
      input.correct = draft.correctBool
      break
    case 'short': {
      const accepted = draft.accepted.map((a) => a.trim())
      if (accepted.length === 0 || accepted.some((a) => a === '')) {
        fields.correct = 'Accepted answers must not be empty.'
      }
      input.correct = { accepted }
      break
    }
  }
  return Object.keys(fields).length > 0 ? { fields } : { input }
}

/**
 * A freshly added question must already be valid (the server validates every
 * save), so each type starts as the smallest sensible complete question.
 */
export function newQuestionInput(type: QuestionType): QuestionInput {
  const base: QuestionInput = {
    type,
    body: { text: 'Untitled question' },
    correct: null,
    points: 1,
  }
  switch (type) {
    case 'single':
    case 'multi':
      base.options = [
        { key: 'a', text: 'Option A' },
        { key: 'b', text: 'Option B' },
      ]
      base.correct = type === 'single' ? 'a' : ['a']
      break
    case 'truefalse':
      base.correct = true
      break
    case 'short':
      base.correct = { accepted: ['Answer'] }
      break
  }
  return base
}
