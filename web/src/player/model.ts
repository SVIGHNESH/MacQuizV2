import type { components } from '../api/schema'

export type AssignedQuiz = components['schemas']['AssignedQuiz']
export type AttemptSummary = components['schemas']['AttemptSummary']
export type AttemptDetail = components['schemas']['AttemptDetail']
export type AttemptQuestion = components['schemas']['AttemptQuestion']
export type Attempt = components['schemas']['Attempt']
export type AttemptResult = components['schemas']['AttemptResult']
export type ResultQuestion = components['schemas']['ResultQuestion']

export const ASSIGNED_STATUS_LABEL: Record<AssignedQuiz['status'], string> = {
  scheduled: 'Scheduled',
  live: 'Live',
  closed: 'Closed',
}

export const ATTEMPT_STATUS_LABEL: Record<AttemptSummary['status'], string> = {
  in_progress: 'In progress',
  submitted: 'Submitted',
  graded: 'Graded',
  kicked: 'Kicked',
}

const DATE_TIME = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
  hour: 'numeric',
  minute: '2-digit',
})

export function formatWhen(iso: string): string {
  return DATE_TIME.format(new Date(iso))
}

export function formatDuration(seconds: number): string {
  if (seconds % 3600 === 0) {
    const h = seconds / 3600
    return `${h} hour${h === 1 ? '' : 's'}`
  }
  if (seconds >= 60) {
    const m = Math.floor(seconds / 60)
    const s = seconds % 60
    return s === 0 ? `${m} min` : `${m} min ${s} s`
  }
  return `${seconds} s`
}

/** ms remaining as the countdown shows it: h:mm:ss above an hour, m:ss below. */
export function formatRemaining(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000))
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  const pad = (n: number) => String(n).padStart(2, '0')
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`
}

/** The student's response for one question, in the wire shape the grader reads. */
export type ResponseValue = string | string[] | boolean

/** Narrow an autosaved `unknown` response to the shape the question type owns. */
export function coerceResponse(
  type: AttemptQuestion['type'],
  raw: unknown,
): ResponseValue | undefined {
  switch (type) {
    case 'single':
      return typeof raw === 'string' ? raw : undefined
    case 'multi':
      return Array.isArray(raw)
        ? raw.filter((k): k is string => typeof k === 'string')
        : undefined
    case 'truefalse':
      return typeof raw === 'boolean' ? raw : undefined
    case 'short':
      return typeof raw === 'string' ? raw : undefined
  }
}

/** True when the response counts as answered (a blank short answer does not). */
export function isAnswered(value: ResponseValue | undefined): boolean {
  if (value === undefined) return false
  if (typeof value === 'string') return value.trim() !== ''
  if (Array.isArray(value)) return value.length > 0
  return true
}
