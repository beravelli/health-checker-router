import type { ReactNode } from 'react'
import type { Status } from '../lib/types'

const STATUS_LABEL: Record<Status, string> = {
  healthy: 'Healthy',
  degraded: 'Degraded',
  critical: 'Critical',
  unknown: 'Unknown',
}

export function StatusTag({ status, size }: { status: Status; size?: 'lg' }) {
  return (
    <span
      className={`tag status-${status}`}
      style={size === 'lg' ? { fontSize: 12, padding: '5px 14px' } : undefined}
      title={status === 'unknown' ? 'Probe result inconclusive — not the same as unhealthy' : undefined}
    >
      {STATUS_LABEL[status]}
    </span>
  )
}

export function Card({
  children,
  elevation = 'sm',
  className,
  style,
  onClick,
}: {
  children: ReactNode
  elevation?: 'sm' | 'md' | 'lg'
  className?: string
  style?: React.CSSProperties
  onClick?: () => void
}) {
  return (
    <div
      className={`card elev-${elevation}${className ? ` ${className}` : ''}`}
      style={onClick ? { cursor: 'pointer', ...style } : style}
      onClick={onClick}
      // A clickable card must still be reachable without a mouse.
      role={onClick ? 'button' : undefined}
      tabIndex={onClick ? 0 : undefined}
      onKeyDown={
        onClick
          ? (e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault()
                onClick()
              }
            }
          : undefined
      }
    >
      {children}
    </div>
  )
}

export function Segmented<T extends string>({
  name,
  options,
  value,
  onChange,
  disabled,
  fullWidth,
}: {
  name: string
  options: { id: T; label: string }[]
  value: T
  onChange: (id: T) => void
  disabled?: boolean
  fullWidth?: boolean
}) {
  return (
    <div
      className="seg"
      style={fullWidth ? { width: '100%', flexWrap: 'wrap' } : undefined}
      role="radiogroup"
    >
      {options.map((o) => (
        <label key={o.id} className="seg-opt" style={disabled ? { opacity: 0.6 } : undefined}>
          <input
            type="radio"
            name={name}
            checked={value === o.id}
            disabled={disabled}
            onChange={() => onChange(o.id)}
          />
          {o.label}
        </label>
      ))}
    </div>
  )
}

export function Field({
  label,
  hint,
  children,
}: {
  label: string
  hint?: ReactNode
  children: ReactNode
}) {
  return (
    <div className="field">
      <label>{label}</label>
      {children}
      {hint ? <span className="field-hint">{hint}</span> : null}
    </div>
  )
}

export function Dialog({
  title,
  onClose,
  children,
  footer,
}: {
  title: string
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
}) {
  return (
    <div className="backdrop" onClick={onClose} role="presentation">
      <div
        className="dialog"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-label={title}
      >
        <h3 style={{ marginBottom: 12 }}>{title}</h3>
        {children}
        <div style={{ display: 'flex', gap: 10, marginTop: 20, justifyContent: 'flex-end' }}>
          {footer ?? (
            <button className="btn btn-secondary" onClick={onClose}>
              Close
            </button>
          )}
        </div>
      </div>
    </div>
  )
}

export function Loading({ label = 'Loading' }: { label?: string }) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '40px 0' }}>
      <span className="spinner" />
      <span className="text-muted">{label}…</span>
    </div>
  )
}

export function ErrorNote({ error, onRetry }: { error: Error; onRetry?: () => void }) {
  return (
    <Card elevation="sm" style={{ background: 'var(--color-critical-bg)' }}>
      <div className="card-kicker" style={{ color: 'var(--color-critical-fg)' }}>
        Something went wrong
      </div>
      <p style={{ color: 'var(--color-critical-fg)', margin: '4px 0 12px' }}>{error.message}</p>
      {onRetry ? (
        <button className="btn btn-secondary" onClick={onRetry}>
          Try again
        </button>
      ) : null}
    </Card>
  )
}

export function EmptyNote({ children }: { children: ReactNode }) {
  return (
    <Card elevation="sm">
      <p className="text-muted" style={{ margin: 0 }}>
        {children}
      </p>
    </Card>
  )
}

/** Renders a relative time, falling back to the raw value if unparseable. */
export function relativeTime(iso: string): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return iso

  const seconds = Math.round((Date.now() - then) / 1000)
  if (seconds < 0) return 'just now'
  if (seconds < 60) return `${seconds}s ago`

  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`

  const hours = Math.round(minutes / 60)
  if (hours < 24) return `${hours}h ago`

  const days = Math.round(hours / 24)
  return days === 1 ? 'yesterday' : `${days} days ago`
}

export function clockTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
}
