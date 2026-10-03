import { useEffect, useState } from 'react';
import { App as AntApp, Badge, Button, Layout, Menu, Space, Tag, Typography } from 'antd';
import { SettingOutlined } from '@ant-design/icons';
import { Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom';

import { Playground } from './pages/Playground';
import { Fleet } from './pages/Fleet';
import { Models } from './pages/Models';
import { Cluster } from './pages/Cluster';
import { Tenancy } from './pages/Tenancy';
import { Cost } from './pages/Cost';
import { Settings } from './pages/Settings';
import { listModels, fleetStatus } from './api/gateway';
import { usePoll } from './hooks';
import type { Model } from './types';

const { Header, Content } = Layout;
const { Text } = Typography;

const NAV = [
  { key: '/playground', label: 'Playground' },
  { key: '/fleet', label: 'Fleet' },
  { key: '/tenancy', label: 'Tenancy' },
  { key: '/cost', label: 'Cost' },
  { key: '/models', label: 'Models' },
  { key: '/cluster', label: 'Cluster' },
];

export function App() {
  return (
    <AntApp>
      <Shell />
    </AntApp>
  );
}

function Shell() {
  const location = useLocation();
  const navigate = useNavigate();
  const [settingsOpen, setSettingsOpen] = useState(false);

  // A stable identity for the poll: an inline arrow would restart the timer on
  // every render, which is the same reason usePoll keeps it in a ref.
  const probe = (signal: AbortSignal) => listModels(signal);
  const models$ = usePoll(probe, 10000);
  const status$ = usePoll((signal) => fleetStatus(signal), 20000);

  // React 18+ runs an effect twice in development, and the first run's abort
  // would otherwise leave the poll permanently errored on a remount.
  useEffect(() => {
    if (models$.error && models$.data === undefined) {
      // Nothing to do: the badge already reports the failure.
    }
  }, [models$.error, models$.data]);

  const models: Model[] = models$.data ?? [];
  const selected = location.pathname === '/' ? '/playground' : location.pathname;

  return (
    <Layout style={{ height: '100vh' }}>
      <Header
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 24,
          padding: '0 20px',
          background: '#fff',
          borderBottom: '1px solid rgba(5,5,5,0.08)',
          height: 52,
          lineHeight: '52px',
        }}
      >
        <Space size={10}>
          <div
            style={{
              width: 22,
              height: 22,
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
          <Text strong style={{ fontSize: 15 }}>
            Fleet
          </Text>
          {status$.data && (
            <Tag color={status$.data.edition === 'enterprise' ? 'green' : 'default'} style={{ fontSize: 11 }}>
              {status$.data.edition}
            </Tag>
          )}
        </Space>

        <Menu
          mode="horizontal"
          selectedKeys={[selected]}
          items={NAV}
          onClick={(e) => navigate(e.key)}
          style={{ flex: 1, minWidth: 0, borderBottom: 'none', lineHeight: '51px' }}
        />

        <Space>
          <Badge
            status={models$.error ? 'error' : models$.loading ? 'default' : models.length ? 'success' : 'warning'}
            text={
              <Text type="secondary" style={{ fontSize: 12 }}>
                {models$.error
                  ? 'gateway unreachable'
                  : models$.loading
                    ? 'connecting'
                    : `${models.length} model${models.length === 1 ? '' : 's'}`}
              </Text>
            }
          />
          <Button type="text" icon={<SettingOutlined />} onClick={() => setSettingsOpen(true)} />
        </Space>
      </Header>

      <Content style={{ overflowY: 'auto', padding: selected === '/playground' ? 0 : 20, height: 'calc(100vh - 52px)' }}>
        <Routes>
          <Route path="/" element={<Navigate to="/playground" replace />} />
          <Route
            path="/playground"
            element={
              models$.error ? (
                <div style={{ padding: 20 }}>
                  <GatewayDown detail={models$.error.message} />
                </div>
              ) : (
                <Playground models={models} />
              )
            }
          />
          <Route path="/fleet" element={<Fleet />} />
          <Route path="/models" element={<Models />} />
          <Route path="/cluster" element={<Cluster />} />
          <Route path="/tenancy" element={<Tenancy />} />
          <Route path="/cost" element={<Cost />} />
          <Route path="*" element={<Navigate to="/playground" replace />} />
        </Routes>
      </Content>

      <Settings open={settingsOpen} onClose={() => setSettingsOpen(false)} />
    </Layout>
  );
}

function GatewayDown({ detail }: { detail: string }) {
  return (
    <div style={{ maxWidth: 560, margin: '80px auto', textAlign: 'center' }}>
      <Text strong style={{ fontSize: 16, display: 'block', marginBottom: 8 }}>
        The gateway is not answering
      </Text>
      <Text type="secondary">{detail}</Text>
      <div style={{ marginTop: 16 }}>
        <Text type="secondary" style={{ fontSize: 12 }}>
          Start it with <Text code>fleet-gateway --demo</Text>
        </Text>
      </div>
    </div>
  );
}
