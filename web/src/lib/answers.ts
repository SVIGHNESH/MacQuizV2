import type { components } from '../api/schema'

type ResultQuestion = components['schemas']['ResultQuestion']

/**
 * Formatting for a graded ResultQuestion's response and answer key, shared by
 * the student's released review (player/ResultReview) and the teacher's
 * per-attempt review drawer (authoring/TeacherResultsPanel). Both surfaces
 * read the same wire shape, so the "B · Encrypted vault" rendering rules
 * live once, here.
 */

export function keysOf(raw: unknown): string[] {
  if (typeof raw === 'string') return [raw]
  if (Array.isArray(raw)) return raw.filter((k): k is string => typeof k === 'string')
  return []
}

/**
 * "B · Encrypted vault" - the option letter, then what it said. A review
 * table never lists the options themselves, so a bare letter would be
 * unreadable; every key carries its text, including each key of a multi.
 */
export function labelForKeys(question: ResultQuestion, keys: string[]): string {
  return keys
    .map((key) => {
      const option = (question.options ?? []).find((o) => o.key === key)
      const letter = key.toUpperCase()
      return option ? `${letter} · ${option.text}` : letter
    })
    .join(', ')
}

/** What the student put down, in the shape their question type takes. */
export function responseText(question: ResultQuestion): string {
  switch (question.type) {
    case 'single':
    case 'multi': {
      const keys = keysOf(question.response)
      return keys.length ? labelForKeys(question, keys) : ''
    }
    case 'truefalse':
      return typeof question.response === 'boolean'
        ? question.response
          ? 'True'
          : 'False'
        : ''
    case 'short':
      return typeof question.response === 'string' ? question.response.trim() : ''
  }
}

/** The answer key, rendered in the shape the question type takes. */
export function correctAnswerText(question: ResultQuestion): string {
  switch (question.type) {
    case 'single':
    case 'multi':
      return labelForKeys(question, keysOf(question.correct))
    case 'truefalse':
      return question.correct === true
        ? 'True'
        : question.correct === false
          ? 'False'
          : ''
    case 'short': {
      const accepted = (question.correct as { accepted?: unknown })?.accepted
      return keysOf(accepted).join(', ')
    }
  }
}

/** The label the answer key renders under, per question type. */
export function correctAnswerLabel(question: ResultQuestion): string {
  return question.type === 'short'
    ? 'Accepted'
    : question.type === 'multi'
      ? 'Correct answers'
      : 'Correct answer'
}
