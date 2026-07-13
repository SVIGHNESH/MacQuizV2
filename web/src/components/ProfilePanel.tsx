import { useEffect, useRef, useState } from 'react'
import { api } from '../api/client'
import { useAuth, AuthActionError, type SessionUser } from '../auth/context'
import Avatar from './Avatar'
import { AVATAR_PRESETS } from './avatarPresets'

const ROLE_LABEL: Record<string, string> = {
  admin: 'Admin',
  teacher: 'Teacher',
  student: 'Student',
}

const UPLOAD_TYPES = ['image/png', 'image/jpeg', 'image/webp', 'image/gif']
const MAX_UPLOAD_BYTES = 2 * 1024 * 1024

/**
 * The profile page every role reaches from its rail identity. Identity is
 * admin-issued on an exam platform, so name/email/role render read-only;
 * the editable surface is the avatar (sticker or photo) plus a change
 * password form that reuses the auth flow.
 */
export default function ProfilePanel({ user }: { user: SessionUser }) {
  const { updateUser } = useAuth()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const fileInput = useRef<HTMLInputElement>(null)
  // A picked file waits in preview until the user confirms: the preview's
  // CSS center-crop square is exactly what the server stores, so what you
  // approve is what everyone sees.
  const [pending, setPending] = useState<{ file: File; url: string } | null>(null)
  useEffect(() => {
    return () => {
      if (pending) URL.revokeObjectURL(pending.url)
    }
  }, [pending])

  const selectedPreset = user.avatar?.startsWith('preset:')
    ? user.avatar.slice('preset:'.length)
    : null

  const applyPreset = async (slug: string) => {
    if (busy) return
    setBusy(true)
    setError('')
    const result = await api
      .POST('/api/v1/auth/me/avatar/preset', { body: { preset: slug } })
      .catch(() => null)
    setBusy(false)
    if (!result?.data) {
      setError(result?.error?.message ?? 'Could not save the avatar. Try again.')
      return
    }
    updateUser(result.data.user)
  }

  const surpriseMe = () => {
    const others = AVATAR_PRESETS.filter((p) => p.slug !== selectedPreset)
    const pick = others[Math.floor(Math.random() * others.length)]!
    void applyPreset(pick.slug)
  }

  const onPickFile = (file: File | undefined) => {
    setError('')
    if (!file) return
    if (!UPLOAD_TYPES.includes(file.type)) {
      setError('The photo must be a PNG, JPEG, WebP, or GIF image.')
      return
    }
    if (file.size > MAX_UPLOAD_BYTES) {
      setError('The photo must be 2 MB or smaller.')
      return
    }
    setPending({ file, url: URL.createObjectURL(file) })
  }

  const confirmUpload = async () => {
    if (!pending || busy) return
    setBusy(true)
    setError('')
    // Same raw-body convention as the bulk imports: no multipart envelope,
    // the request body is the file and the server re-encodes it anyway.
    const result = await api
      .PUT('/api/v1/auth/me/avatar', {
        headers: { 'Content-Type': 'application/octet-stream' },
        bodySerializer: (body) => body,
        body: pending.file as unknown as string,
      })
      .catch(() => null)
    setBusy(false)
    if (!result?.data) {
      setError(result?.error?.fields?.file ?? result?.error?.message ?? 'Upload failed. Try again.')
      return
    }
    setPending(null)
    updateUser(result.data.user)
  }

  const removeAvatar = async () => {
    if (busy) return
    setBusy(true)
    setError('')
    const result = await api.DELETE('/api/v1/auth/me/avatar').catch(() => null)
    setBusy(false)
    if (!result?.data) {
      setError(result?.error?.message ?? 'Could not remove the avatar. Try again.')
      return
    }
    updateUser(result.data.user)
  }

  return (
    <section className="profile" aria-label="Profile">
      <header className="page-head">
        <div>
          <p className="eyebrow">Profile</p>
          <h1 className="page-title">Your profile</h1>
        </div>
      </header>

      <div className="card profile-identity" data-testid="profile-identity">
        <Avatar userId={user.id} fullName={user.full_name} avatar={user.avatar} size="large" />
        <div className="identity-text">
          <p className="identity-name">{user.full_name}</p>
          <p className="identity-email">{user.email}</p>
        </div>
        <span className="chip chip-role">{ROLE_LABEL[user.role] ?? user.role}</span>
      </div>

      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}

      <div className="card profile-section">
        <div className="profile-section-head">
          <h2 className="card-title">Pick a sticker</h2>
          <button
            className="button button-quiet"
            type="button"
            disabled={busy}
            onClick={surpriseMe}
            data-testid="avatar-surprise"
          >
            Surprise me
          </button>
        </div>
        <div className="preset-grid" role="listbox" aria-label="Avatar stickers">
          {AVATAR_PRESETS.map((preset) => (
            <button
              key={preset.slug}
              type="button"
              role="option"
              aria-selected={selectedPreset === preset.slug}
              className={`preset-tile${selectedPreset === preset.slug ? ' preset-tile-selected' : ''}`}
              disabled={busy}
              onClick={() => void applyPreset(preset.slug)}
              data-testid={`avatar-preset-${preset.slug}`}
            >
              <Avatar
                userId={user.id}
                fullName={user.full_name}
                avatar={`preset:${preset.slug}`}
              />
              <span className="preset-name">{preset.name}</span>
            </button>
          ))}
        </div>
      </div>

      <div className="card profile-section">
        <div className="profile-section-head">
          <h2 className="card-title">Or use a photo</h2>
          {user.avatar && (
            <button
              className="button button-quiet"
              type="button"
              disabled={busy}
              onClick={() => void removeAvatar()}
              data-testid="profile-remove-avatar"
            >
              Remove avatar
            </button>
          )}
        </div>
        {pending ? (
          <div className="profile-upload-preview">
            <img className="profile-preview-photo" src={pending.url} alt="Photo preview" />
            <div className="profile-preview-actions">
              <p className="profile-hint">
                It will be shown as this square, at the center of your photo.
              </p>
              <div className="profile-preview-buttons">
                <button
                  className="button button-primary"
                  type="button"
                  disabled={busy}
                  onClick={() => void confirmUpload()}
                  data-testid="avatar-upload-confirm"
                >
                  {busy ? 'Uploading…' : 'Use this photo'}
                </button>
                <button
                  className="button button-quiet"
                  type="button"
                  disabled={busy}
                  onClick={() => setPending(null)}
                >
                  Cancel
                </button>
              </div>
            </div>
          </div>
        ) : (
          <div className="profile-upload">
            <button
              className="button"
              type="button"
              disabled={busy}
              onClick={() => fileInput.current?.click()}
              data-testid="avatar-upload-button"
            >
              Upload photo
            </button>
            <p className="profile-hint">PNG, JPEG, WebP, or GIF, up to 2 MB.</p>
            <input
              ref={fileInput}
              type="file"
              accept={UPLOAD_TYPES.join(',')}
              hidden
              data-testid="avatar-upload-input"
              onChange={(e) => {
                onPickFile(e.target.files?.[0])
                e.target.value = ''
              }}
            />
          </div>
        )}
      </div>

      <ChangePasswordCard email={user.email} />
    </section>
  )
}

/**
 * The self-service password change, reusing the auth-context action (which
 * silently re-logs-in, since the server revokes every session on success).
 */
function ChangePasswordCard({ email }: { email: string }) {
  const { changePassword } = useAuth()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')
  const [done, setDone] = useState(false)

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (submitting) return
    setError('')
    setDone(false)
    if (next.length < 10) {
      setError('The new password must be at least 10 characters.')
      return
    }
    setSubmitting(true)
    try {
      await changePassword(current, next)
      setCurrent('')
      setNext('')
      setDone(true)
    } catch (err) {
      setError(
        err instanceof AuthActionError && err.code === 'INVALID_CREDENTIALS'
          ? 'The current password is incorrect.'
          : 'Could not change the password. Try again.',
      )
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="card profile-section">
      <h2 className="card-title">Change password</h2>
      <form className="form profile-password-form" onSubmit={(e) => void onSubmit(e)} noValidate>
        {/* Lets password managers know which account this change is for. */}
        <input type="email" value={email} autoComplete="username" hidden readOnly />
        <div className="field">
          <label className="field-label" htmlFor="profile-current-password">
            Current password
          </label>
          <input
            id="profile-current-password"
            className="input"
            type="password"
            autoComplete="current-password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
        </div>
        <div className="field">
          <label className="field-label" htmlFor="profile-new-password">
            New password
          </label>
          <input
            id="profile-new-password"
            className="input"
            type="password"
            autoComplete="new-password"
            value={next}
            onChange={(e) => setNext(e.target.value)}
          />
        </div>
        {error && (
          <p className="form-error" role="alert">
            {error}
          </p>
        )}
        {done && (
          <p className="profile-password-done" role="status">
            Password changed.
          </p>
        )}
        <button className="button button-primary" type="submit" disabled={submitting}>
          {submitting ? 'Changing…' : 'Change password'}
        </button>
      </form>
    </div>
  )
}
