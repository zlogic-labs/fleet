import { Button, Typography } from 'antd';
import { MenuFoldOutlined, MenuUnfoldOutlined } from '@ant-design/icons';

const { Text } = Typography;

/**
 * The strip above the content: the fold toggle and the name of the page.
 *
 * The toggle disappears below the layout breakpoint. The Sider is pinned folded
 * there -- 224px of a 400px window leaves 176px for content, which is how a
 * table ends up one character per line -- so a button that would appear to work
 * and change nothing is worse than no button.
 */
export function Bar({
  folded,
  narrow,
  onToggle,
  title,
}: {
  folded: boolean;
  narrow: boolean;
  onToggle: () => void;
  title: string;
}) {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 12,
        height: 52,
        padding: '0 16px 0 8px',
        background: '#fff',
        borderBottom: '1px solid rgba(5,5,5,0.08)',
        flex: '0 0 auto',
      }}
    >
      {!narrow && (
        <Button
          type="text"
          onClick={onToggle}
          icon={folded ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
          aria-label={folded ? 'Expand the sidebar' : 'Collapse the sidebar'}
        />
      )}
      <Text strong style={{ fontSize: 15 }}>
        {title}
      </Text>
    </div>
  );
}