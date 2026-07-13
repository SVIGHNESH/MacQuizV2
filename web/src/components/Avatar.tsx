import { useEffect, useState, type CSSProperties } from 'react'
import { presetBySlug } from './avatarPresets'
import { initials } from '../lib/initials'

interface AvatarProps {
  userId: string
  fullName: string
  /** The User.avatar value: "preset:<slug>", "upload:<hash>", or null/absent. */
  avatar?: string | null
  size?: 'small' | 'regular' | 'large'
  /**
   * The leaderboard's dark island reserves the slate ramp (docs/11); the
   * initials chip flips to it there instead of the light tint pair.
   */
  dark?: boolean
}

/**
 * The one avatar renderer for every identity surface: an uploaded photo
 * (served by GET /users/{id}/avatar, cache-busted by the content hash), a
 * preset sticker, or the initials chip - in that order of preference, and
 * falling back a step when the photo 404s (e.g. an admin just cleared it).
 */
export default function Avatar({ userId, fullName, avatar, size = 'regular', dark = false }: AvatarProps) {
  const [photoFailed, setPhotoFailed] = useState(false)
  useEffect(() => {
    setPhotoFailed(false)
  }, [avatar])

  const sizeClass = size === 'small' ? ' avatar-small' : size === 'large' ? ' avatar-large' : ''
  const darkClass = dark ? ' avatar-dark' : ''

  const uploadHash = avatar?.startsWith('upload:') ? avatar.slice('upload:'.length) : null
  if (uploadHash && !photoFailed) {
    return (
      <span className={`avatar${sizeClass}${darkClass}`} aria-hidden="true">
        <img
          className="avatar-photo"
          src={`/api/v1/users/${userId}/avatar?v=${uploadHash}`}
          alt=""
          onError={() => setPhotoFailed(true)}
        />
      </span>
    )
  }

  const preset = avatar?.startsWith('preset:')
    ? presetBySlug(avatar.slice('preset:'.length))
    : undefined
  if (preset) {
    const style = {
      background: `var(${preset.bg})`,
      color: `var(${preset.fg})`,
      '--avatar-bg': `var(${preset.bg})`,
    } as CSSProperties
    return (
      <span className={`avatar${sizeClass}`} style={style} aria-hidden="true" data-avatar-preset={preset.slug}>
        <svg viewBox="0 0 64 64" role="img" aria-label={preset.name}>
          {preset.art}
        </svg>
      </span>
    )
  }

  return (
    <span className={`avatar${sizeClass}${darkClass}`} aria-hidden="true">
      {initials(fullName)}
    </span>
  )
}
