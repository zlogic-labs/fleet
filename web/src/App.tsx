import { useCallback } from 'react';
import { App as AntApp, Badge, Typography } from 'antd';
import { Navigate, Route, Routes } from 'react-router-dom';

import { Shell } from './layout/Shell';
import { navPaths } from './layout/nav';
import { Playground } from './pages/Playground';
import { Fleet } from './pages/Fleet';
import { Models } from './pages/Models';
import { Cluster } from './pages/Cluster';
import { Tenancy } from './pages/Tenancy';
import { Cost } from './pages/Cost';
import { listModels, fleetStatus } from './api/gateway';
import { usePoll } from './hooks';
import type { Model } from './types';

const { Text } = Typography;

// A page that exists but is not in the sidebar is a page nobody finds, and a
// sidebar entry that goes nowhere is worse. Both are checked at startup
// because neither is a crash: the console looks fine either way, which is
// exactly why this needs to be asserted rather than noticed.
const ROUTES = ['/playground', '/fleet', '/models', '/cluster', '/tenancy', '/cost'];

function checkNavigation() {
  const nav = navPaths();
  for (const path of ROUTES) {
    if (!nav.includes(path)) {
      throw new Error(`console: ${path} is a route but not in the navigation`);
    }
  }
  for (const path of nav) {
    if (!ROUTES.includes(path)) {
      throw new Error(`console: ${path} is in the navigation but has no route`);
    }
  }
}
checkNavigation();

export function App() {
  return (
    <AntApp>
      <ShellFrame />
    </AntApp>
  );
}

function ShellFrame() {
  // A stable identity for the poll: an inline arrow would restart the timer on
  // every render, which is the same reason usePoll keeps it in a ref.
  const probe = useCallback((signal: AbortSignal) => listModels(signal), []);
  const models$ = usePoll(probe, 10000);
  const status$ = usePoll((signal) => fleetStatus(signal), 20000);

  const models: Model[] = models$.data ?? [];

  return (
    <Shell
      edition={status$.data?.edition}
      status={
        <Badge
          status={
            models$.error ? 'error' : models$.loading ? 'default' : models.length ? 'success' : 'warning'
          }
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
      }
    >
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
    </Shell>
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