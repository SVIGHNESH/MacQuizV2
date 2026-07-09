import { useState, type FormEvent } from 'react'
import { AuthActionError, useAuth } from '../auth/context'

function loginErrorMessage(err: AuthActionError): string {
  switch (err.code) {
    case 'INVALID_CREDENTIALS':
      return 'That email and password combination is not right. Check both and try again.'
    case 'RATE_LIMITED':
      return err.retryAfterSeconds
        ? `Too many attempts. Wait ${err.retryAfterSeconds} seconds, then try again.`
        : 'Too many attempts. Wait a minute, then try again.'
    case 'VALIDATION_FAILED':
      return 'Enter your email address and password.'
    default:
      return err.message
  }
}

export default function LoginScreen() {
  const { login } = useAuth()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  async function onSubmit(event: FormEvent) {
    event.preventDefault()
    if (submitting) return
    setSubmitting(true)
    setError(null)
    try {
      await login(email, password)
    } catch (err) {
      setError(
        err instanceof AuthActionError
          ? loginErrorMessage(err)
          : 'Something went wrong. Try again.',
      )
      setSubmitting(false)
    }
  }

  return (
    <main className="shell">
      <section className="card auth-card">
        <span className="brand-mark brand-mark-auth" aria-hidden="true">
          M
        </span>
        <header className="auth-heading">
          <h1 className="page-title">Sign in to MacQuiz</h1>
          <p className="auth-subtitle">
            Use the credentials your administrator issued. Accounts aren't
            self-created.
          </p>
        </header>

        <form className="form" onSubmit={onSubmit} noValidate>
          <div className="field">
            <label className="field-label" htmlFor="login-email">
              Email
            </label>
            <input
              id="login-email"
              className="input"
              type="email"
              autoComplete="username"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoFocus
              required
            />
          </div>

          <div className="field">
            <label className="field-label" htmlFor="login-password">
              Password
            </label>
            <input
              id="login-password"
              className="input"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </div>

          {error && (
            <p className="form-error" role="alert">
              {error}
            </p>
          )}

          <button
            className="button button-primary auth-submit"
            type="submit"
            disabled={submitting}
          >
            {submitting ? 'Signing in…' : 'Sign in'}
          </button>

          {/* No self-serve reset exists (docs/08): the only recovery path is
              an administrator reissuing a one-time credential. */}
          <p className="form-footnote">
            First time here? Sign in with the one-time password your
            administrator gave you; you will pick your own right after. Locked
            out? Ask your administrator.
          </p>
        </form>
      </section>
    </main>
  )
}
