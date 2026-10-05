// The console shell: a collapsible sidebar and a scrolling content area.
//
// Built from antd's own Layout.Sider and Menu rather than a layout package. A
// layout component would have brought a second form library, an UmiJS routing
// convention and a second copy of the icon set, to reproduce a collapsed menu
// and a header bar. antd 6 has both, and nested submenus are one field on
// `items`.

import { useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import { Button, Layout, Menu, Space, Tag, Tooltip, Typography } from 'antd';
import {
  ApiOutlined,
  CloudServerOutlined,
  DashboardOutlined,
  DollarOutlined,
  SettingOutlined,
} from '@ant-design/icons';
import { useLocation, useNavigate } from 'react-router-dom';

import { Bar } from './Bar';
import { HOME, NAV, SETTINGS, groupOf } from './nav';
import { Settings } from '../pages/Settings';

const { Sider, Content } = Layout;
const { Text } = Typography;

const GROUP_ICON: Record<string, ReactNode> = {
  serving: <ApiOutlined />,
  infrastructure: <CloudServerOutlined />,
  billing: <DollarOutlined />,
};

export interface ShellProps {
  children: ReactNode;
  edition?: string;
  status: ReactNode;
}

export function Shell({ children, edition, status }: ShellProps) {
  const location = useLocation();
  const navigate = useNavigate();
  const [collapsed, setCollapsed] = useState(false);
  // Below antd's lg breakpoint the sider cannot be 224px: at a 430px window
  // that left 190px of content, which is narrower than the tables it holds and
  // produced the one-character-per-line layout this replaced. antd already
  // watches the width for us, so the state is its answer rather than a
  // matchMedia of our own that could disagree with the one the Sider uses.
  const [narrow, setNarrow] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [open, setOpen] = useState<string[]>([]);

  const folded = collapsed || narrow;

  // '/' is Overview, not Playground. Collapsing the root onto another page
  // made the root a dead entry: a deep link to / served a screen that nothing
  // highlighted, which is the shape of a link that goes nowhere.
  const selected = location.pathname;

  // Open whichever group owns the current route. Deriving this from the route
  // rather than leaving it to defaultOpenKeys is what makes a deep link land on
  // an expanded menu: someone who pastes /cost should see Cost, not a
  // collapsed "Tenancy & billing" they have to guess at.
  useEffect(() => {
    const owner = groupOf(selected)?.key;
    if (owner) {
      setOpen((keys) => (keys.includes(owner) ? keys : [...keys, owner]));
    }
  }, [selected]);

  const items = [
    // Overview first and outside the groups, so the tree below it reads as the
    // detail it summarises.
    { key: HOME.key, label: HOME.label, icon: <DashboardOutlined /> },
    ...NAV.map((group) => ({
      key: group.key,
      label: group.label,
      icon: GROUP_ICON[group.key],
      children: group.items.map((item) => ({ key: item.key, label: item.label })),
    })),
  ];

  return (
    <Layout style={{ height: '100vh' }}>
      <Sider
        collapsible
        collapsed={folded}
        breakpoint="lg"
        collapsedWidth={80}
        onBreakpoint={setNarrow}
        onCollapse={setCollapsed}
        trigger={null}
        theme="light"
        width={224}
        style={{
          borderRight: '1px solid rgba(5,5,5,0.08)',
          display: 'flex',
          flexDirection: 'column',
        }}
      >
        <Brand collapsed={folded} edition={edition} />

        <div style={{ flex: 1, minHeight: 0, overflowY: 'auto' }}>
          <Menu
            mode="inline"
            selectedKeys={[selected]}
            openKeys={folded ? undefined : open}
            onOpenChange={setOpen}
            onClick={(e) => navigate(e.key)}
            items={items}
            style={{ borderInlineEnd: 'none' }}
          />
        </div>

        <Foot
          collapsed={folded}
          onSettings={() => setSettingsOpen(true)}
          status={status}
        />
      </Sider>

      {/* minWidth 0, not for tidiness. A flex child defaults to min-width
          auto, so one wide descendant -- a Card whose title and extra share a
          row is enough -- pushes this column past the viewport and the content
          is clipped at the right edge with no scrollbar to explain why. */}
      <Layout style={{ minWidth: 0 }}>
        <Bar folded={folded} narrow={narrow} onToggle={() => setCollapsed((c) => !c)} title={titleFor(selected)} />
        <Content style={{ overflowY: 'auto', padding: selected === '/playground' ? 0 : 20 }}>
          {children}
        </Content>
      </Layout>

      <Settings open={settingsOpen} onClose={() => setSettingsOpen(false)} />
    </Layout>
  );
}

function Brand({ collapsed, edition }: { collapsed: boolean; edition?: string }) {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 10,
        height: 52,
        padding: collapsed ? '0 20px' : '0 16px',
        borderBottom: '1px solid rgba(5,5,5,0.06)',
        overflow: 'hidden',
      }}
    >
      <div
        style={{
          width: 22,
          height: 22,
          flex: '0 0 auto',
          borderRadius: 6,
          background: '#1677ff',
          color: '#fff',
          fontWeight: 700,
          fontSize: 13,
          display: 'grid',
          placeItems: 'center',
        }}
      >
        F
      </div>
      {!collapsed && (
        <Space size={8}>
          <Text strong style={{ fontSize: 15, whiteSpace: 'nowrap' }}>
            Fleet
          </Text>
          {edition && (
            <Tag
              color={edition === 'enterprise' ? 'green' : 'default'}
              style={{ fontSize: 11, marginInlineEnd: 0 }}
            >
              {edition}
            </Tag>
          )}
        </Space>
      )}
    </div>
  );
}

function Foot({
  collapsed,
  onSettings,
  status,
}: {
  collapsed: boolean;
  onSettings: () => void;
  status: ReactNode;
}) {
  const gear = (
    <Button
      type="text"
      icon={<SettingOutlined />}
      onClick={onSettings}
      aria-label={SETTINGS.label}
      style={{ width: collapsed ? '100%' : undefined }}
    />
  );
  return (
    <div
      style={{
        borderTop: '1px solid rgba(5,5,5,0.06)',
        padding: collapsed ? '8px 12px' : '8px 16px',
      }}
    >
      {!collapsed && <div style={{ marginBottom: 4 }}>{status}</div>}
      {collapsed ? (
        <Tooltip title={SETTINGS.label} placement="right">
          {gear}
        </Tooltip>
      ) : (
        gear
      )}
    </div>
  );
}

function titleFor(path: string): string {
  if (path === HOME.key) return HOME.label;
  for (const group of NAV) {
    const item = group.items.find((i) => i.key === path);
    if (item) {
      return item.label;
    }
  }
  return 'Fleet';
}