import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { ConfigProvider, theme } from 'antd';

import { App } from './App';
import './index.css';

// The console is an operations tool: a compact layout and a calm palette
// matter more than a generous one, because the tables carry the information.
const themeConfig = {
  algorithm: theme.defaultAlgorithm,
  token: {
    colorPrimary: '#1677ff',
    colorInfo: '#1677ff',
    borderRadius: 6,
    fontSize: 13,
    colorBgLayout: '#f5f6f8',
    fontFamily:
      '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif',
  },
  components: {
    Layout: { headerHeight: 52, headerPadding: '0 20px' },
    Table: { cellPaddingBlock: 8, cellPaddingInline: 10 },
    Card: { paddingLG: 16 },
    Statistic: { titleFontSize: 12 },
  },
};

const root = document.getElementById('root');
if (!root) throw new Error('#root is missing from index.html');

createRoot(root).render(
  <StrictMode>
    <ConfigProvider theme={themeConfig}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </ConfigProvider>
  </StrictMode>,
);
