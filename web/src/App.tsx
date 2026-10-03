import { useCallback } from 'react';
import { App as AntApp, Badge, Typography } from 'antd';
import { Navigate, Route, Routes } from 'react-router-dom';

import { Shell } from './layout/Shell';
import { navPaths } from './layout/nav';
import { Overview } from './pages/Overview';
import { Playground } from './pages/Playground';
import { Fleet } from './pages/Fleet';
import { Models } from './pages/Models';
import { Cluster } from './pages/Cluster';
import { Tenancy } from './pages/Tenancy';
import { Cost } from './pages/Cost';
import { listModels, fleetStatus } from './api/gateway';
import { adoptControlPlane } from './api/client';
import { usePoll } from './hooks';
import type { Model } from './types';

const { Text } = Typography;

// A page that exists but is not in the sidebar is a page nobody finds, and a
// sidebar entry that goes nowhere is worse. Both are checked at startup
// because neither is a crash: the console looks fine either way, which is
// exactly why this needs to be asserted rather than noticed.
const ROUTES = ['/', '/playground', '/endpoints', '/models', '/cluster', '/tenancy', '/cost'];

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

  // The gateway is the one process that knows where the control plane is, and
  // it serves this page. Asking it here means a deployment whose two processes
  // are on different hosts needs nobody to open Settings first — which is what
  // a hardcoded localhost cost every such deployment.
  adoptControlPlane(status$.data?.controlPlane);

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
        <Route path="/" element={<Overview />} />
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
        <Route path="/endpoints" element={<Fleet />} />
        {/* The page used to be called Fleet, which named the product rather
            than the thing on screen. Kept as a redirect so a shared link from
            an earlier build still lands on the endpoints it described. */}
        <Route path="/fleet" element={<Navigate to="/endpoints" replace />} />
        <Route path="/models" element={<Models />} />
        <Route path="/cluster" element={<Cluster />} />
        <Route path="/tenancy" element={<Tenancy />} />
        <Route path="/cost" element={<Cost />} />
        {/* Overview, not the playground: an address that matches nothing should
            land somewhere that explains the deployment. */}
        <Route path="*" element={<Navigate to="/" replace />} />
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