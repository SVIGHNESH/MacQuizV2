import { useState, type FormEvent } from 'react'
import { AuthActionError, useAuth, type SessionUser } from '../auth/context'

// Mirrors the server rule in authusers/http.go (VALIDATION_FAILED under 10).
const MIN_PASSWORD_LENGTH = 10

function changeErrorMessage(err: AuthActionError): string {
  switch (err.code) {
    case 'INVALID_CREDENTIALS':
      return 'The current password is not right. It is the one you just signed in with.'
    case 'VALIDATION_FAILED':
      return (
        err.fields?.new_password ??
        `Pick a new password of at least ${MIN_PASSWORD_LENGTH} characters.`
      )
    default:
      return err.message
  }
}

/**
 * The requirement list, split by what is actually true. Only the length rule
 * and the confirmation match gate the submit - the rest is advice, and it says
 * so, because a checklist that claims "must" about a rule the server never
 * enforces teaches the wrong thing about the account it just created.
 */
function PasswordChecklist({
  password,
  confirmation,
}: {
  password: string
  confirmation: string
}) {
  const required = [
    {
      label: `At least ${MIN_PASSWORD_LENGTH} characters`,
      met: password.length >= MIN_PASSWORD_LENGTH,
    },
    {
      label: 'Both copies match',
      met: password.length > 0 && password === confirmation,
    },
  ]
  const advice = [
    {
      label: 'Upper and lowercase letters',
      met: /[a-z]/.test(password) && /[A-Z]/.test(password),
    },
    { label: 'A number or symbol', met: /[^A-Za-z]/.test(password) },
  ]

  return (
    <div className="checklist">
      <span className="checklist-title">Password must include</span>
      <ul className="checklist-items">
        {required.map((item) => (
          <ChecklistItem key={item.label} {...item} />
        ))}
      </ul>
      <span className="checklist-title checklist-title-advice">
        Stronger if it has
      </span>
      <ul className="checklist-items">
        {advice.map((item) => (
          <ChecklistItem key={item.label} {...item} />
        ))}
      </ul>
    </div>
  )
}

function ChecklistItem({ label, met }: { label: string; met: boolean }) {
  return (
    <li className={`checklist-item${met ? ' checklist-item-met' : ''}`}>
      <span aria-hidden="true">{met ? '✓' : '○'}</span>
      <span>{label}</span>
      {met && <span className="visually-hidden"> - met</span>}
    </li>
  )
}

export default function ChangePasswordScreen({ user }: { user: SessionUser }) {
  const { changePassword, logout } = useAuth()
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  async function onSubmit(event: FormEvent) {
    event.preventDefault()
    if (submitting) return
    if (newPassword.length < MIN_PASSWORD_LENGTH) {
      setError(
        `Pick a new password of at least ${MIN_PASSWORD_LENGTH} characters.`,
      )
      return
    }
    if (newPassword !== confirmPassword) {
      setError('The two copies of the new password do not match.')
      return
    }
    if (newPassword === currentPassword) {
      setError('The new password must differ from the one-time password.')
      return
    }
    setSubmitting(true)
    setError(null)
    try {
      await changePassword(currentPassword, newPassword)
    } catch (err) {
      setError(
        err instanceof AuthActionError
          ? changeErrorMessage(err)
          : 'Something went wrong. Try again.',
      )
      setSubmitting(false)
    }
  }

  return (
    <main className="shell">
      <section className="card auth-card">
        <span className="chip chip-lifecycle chip-lifecycle-scheduled">
          One-time credential
        </span>

        <header className="auth-heading">
          <h1 className="page-title">Set your password</h1>
          <p className="auth-subtitle">
            You are signed in as <strong>{user.email}</strong> with a one-time
            password. Choose your own to continue; the one-time password stops
            working immediately.
          </p>
        </header>

        <form className="form" onSubmit={onSubmit} noValidate>
          <div className="field">
            <label className="field-label" htmlFor="current-password">
              One-time password
            </label>
            <input
              id="current-password"
              className="input"
              type="password"
              autoComplete="current-password"
              value={currentPassword}
              onChange={(e) => setCurrentPassword(e.target.value)}
              autoFocus
              required
            />
          </div>

          <div className="field">
            <label className="field-label" htmlFor="new-password">
              New password
            </label>
            <input
              id="new-password"
              className="input"
              type="password"
              autoComplete="new-password"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              required
            />
          </div>

          <div className="field">
            <label className="field-label" htmlFor="confirm-password">
              New password, again
            </label>
            <input
              id="confirm-password"
              className="input"
              type="password"
              autoComplete="new-password"
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
              required
            />
          </div>

          <PasswordChecklist
            password={newPassword}
            confirmation={confirmPassword}
          />

          {error && (
            <p className="form-error" role="alert">
              {error}
            </p>
          )}

          {/* Ink marks the point of no return: the one-time password dies here. */}
          <button
            className="button button-commit"
            type="submit"
            disabled={submitting}
          >
            {submitting ? 'Saving…' : 'Set password and continue'}
          </button>

          <button
            className="button button-quiet"
            type="button"
            onClick={() => void logout()}
          >
            Sign out instead
          </button>
        </form>
      </section>
    </main>
  )
}
