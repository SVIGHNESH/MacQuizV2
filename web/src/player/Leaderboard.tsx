import type { LeaderboardEntry } from './model'
import Avatar from '../components/Avatar'

/**
 * The dark island (docs/11 section 4, design doc St5): the one sanctioned
 * dark surface in the product. Ranks 1-3 wear gold/silver/bronze gradient
 * medallions and rank 1's row lifts one slate step; the reader's own row is
 * always present, outlined, even when it falls past the leading entries.
 */
export default function Leaderboard({
  quizTitle,
  entries,
  total,
}: {
  quizTitle: string
  entries: LeaderboardEntry[]
  total: number
}) {
  return (
    <section className="leaderboard" aria-label="Leaderboard">
      <header className="leaderboard-head">
        <span className="leaderboard-eyebrow">Leaderboard</span>
        <h2 className="leaderboard-title">{quizTitle}</h2>
      </header>
      <ol className="leaderboard-rows">
        {entries.map((entry) => (
          <Row key={entry.student_id} entry={entry} />
        ))}
      </ol>
      <p className="leaderboard-note">
        Ranks update as attempts are graded · ties broken by time taken
        {total > entries.length && ` · ${total} students ranked`}
      </p>
    </section>
  )
}

function Row({ entry }: { entry: LeaderboardEntry }) {
  const medal = entry.rank <= 3
  const rowClass = [
    'leaderboard-row',
    entry.rank === 1 && 'leaderboard-row-lifted',
    entry.is_self && 'leaderboard-row-self',
  ]
    .filter(Boolean)
    .join(' ')
  return (
    <li className={rowClass}>
      <span
        className={
          medal
            ? `leaderboard-medal leaderboard-medal-${entry.rank}`
            : 'leaderboard-rank tabular'
        }
      >
        {entry.rank}
      </span>
      <span className="leaderboard-name">
        <Avatar
          userId={entry.student_id}
          fullName={entry.full_name}
          avatar={entry.avatar}
          size="small"
          dark
        />
        <span className="leaderboard-name-text">
          {entry.is_self ? `You - ${entry.full_name}` : entry.full_name}
        </span>
      </span>
      <span className="leaderboard-score tabular">
        {entry.accuracy === null
          ? '-'
          : `${Math.round(entry.accuracy * 100)}%`}
      </span>
    </li>
  )
}
