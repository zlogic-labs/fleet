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
  MenuFoldOutlined,
  MenuUnfoldOutlined,
  SettingOutlined,
} from '@ant-design/icons';
import { useLocation, useNavigate } from 'react-router-dom';

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
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [open, setOpen] = useState<string[]>([]);

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
        collapsed={collapsed}
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
        <Brand collapsed={collapsed} edition={edition} />

        <div style={{ flex: 1, minHeight: 0, overflowY: 'auto' }}>
          <Menu
            mode="inline"
            selectedKeys={[selected]}
            openKeys={collapsed ? undefined : open}
            onOpenChange={setOpen}
            onClick={(e) => navigate(e.key)}
            items={items}
            style={{ borderInlineEnd: 'none' }}
          />
        </div>

        <Foot
          collapsed={collapsed}
          onSettings={() => setSettingsOpen(true)}
          status={status}
        />
      </Sider>

      <Layout>
        <Bar collapsed={collapsed} onToggle={() => setCollapsed((c) => !c)} title={titleFor(selected)} />
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

function Bar({
  collapsed,
  onToggle,
  title,
}: {
  collapsed: boolean;
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
      <Button
        type="text"
        onClick={onToggle}
        icon={collapsed ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
        aria-label={collapsed ? 'Expand the sidebar' : 'Collapse the sidebar'}
      />
      <Text strong style={{ fontSize: 15 }}>
        {title}
      </Text>
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