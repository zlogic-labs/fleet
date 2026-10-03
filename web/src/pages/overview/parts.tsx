import type { ReactNode } from 'react';
import { Divider, Typography } from 'antd';
import { Link } from 'react-router-dom';

const { Text } = Typography;

// A section is a heading, a rule, and content.
//
// Not a Card, and that is the whole point of this file. The card-per-statistic
// shape gave every number the same weight and the same box, so a figure nobody
// needed looked as important as the one that decides whether to buy another
// GPU. A rule and some air group the page without pretending each number is its
// own object.

export function Group({
  title,
  note,
  to,
  children,
}: {
  title: string;
  note?: string;
  to?: { path: string; label: string };
  children: ReactNode;
}) {
  return (
    <section style={{ marginTop: 32 }}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 12 }}>
        <Text
          strong
          style={{ fontSize: 12, letterSpacing: '.06em', textTransform: 'uppercase' }}
        >
          {title}
        </Text>
        {note && (
          <Text type="secondary" style={{ fontSize: 12 }}>
            {note}
          </Text>
        )}
        <span style={{ flex: 1 }} />
        {to && (
          <Link to={to.path} style={{ fontSize: 12 }}>
            {to.label} →
          </Link>
        )}
      </div>
      <Divider style={{ margin: '10px 0 18px' }} />
      {children}
    </section>
  );
}

export interface Measure {
  label: string;
  value: ReactNode;
  /** What the number is, in the units it is actually reported in. */
  hint?: string;
}

// A grid of figures without a box around any of them.
export function Measures({ items }: { items: Measure[] }) {
  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: 'repeat(auto-fit, minmax(170px, 1fr))',
        gap: '18px 28px',
      }}
    >
      {items.map((m) => (
        <div key={m.label}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {m.label}
          </Text>
          <div style={{ fontSize: 21, fontWeight: 600, lineHeight: 1.25, marginTop: 3 }}>
            {m.value}
          </div>
          {m.hint && (
            <div style={{ fontSize: 11, color: '#00000073', marginTop: 2 }}>{m.hint}</div>
          )}
        </div>
      ))}
    </div>
  );
}