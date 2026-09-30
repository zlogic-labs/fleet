import { Alert, Button, Drawer, Form, Input, Space, Typography } from 'antd';

import { origins, setApiKey, setOrigin, apiKey } from '../api/client';

const { Paragraph, Text } = Typography;

/**
 * Where the gateway and the control plane live.
 *
 * They are separate settings because they are separate processes: the gateway
 * is the data plane and the control plane is the management API. In the common
 * case the console is served by the gateway itself and the control plane is on
 * 8081, but an operator running them apart needs to say so.
 */
interface SettingsValues {
  gateway: string;
  control: string;
  apiKey?: string;
}

export function Settings({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [form] = Form.useForm<SettingsValues>();

  const save = async (values: SettingsValues) => {
    setOrigin('gateway', values.gateway);
    setOrigin('control', values.control);
    setApiKey(values.apiKey ?? '');
    onClose();
    // A reload is the honest way to re-probe both origins; a partial refresh
    // would leave one page's data from a different server.
    window.location.reload();
  };

  return (
    <Drawer title="Settings" open={open} onClose={onClose} width={460}>
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="These are stored in this browser only"
        description="The API key is never sent anywhere but the address you set here."
      />

      <Form
        form={form}
        layout="vertical"
        initialValues={{
          gateway: origins.gateway || window.location.origin,
          control: origins.control,
          apiKey: apiKey(),
        }}
        onFinish={save}
      >
        <Form.Item
          name="gateway"
          label="Gateway"
          extra="Leave as the page origin when the console is served by the gateway itself."
          rules={[{ required: true }]}
        >
          <Input placeholder="http://127.0.0.1:8080" />
        </Form.Item>

        <Form.Item
          name="control"
          label="Control plane"
          extra="fleet-apiserver: model registry, pulls, cluster status, deployments."
          rules={[{ required: true }]}
        >
          <Input placeholder="http://127.0.0.1:8081" />
        </Form.Item>

        <Form.Item name="apiKey" label="API key" extra="Sent as a bearer token to both.">
          <Input.Password placeholder="sk-..." autoComplete="off" />
        </Form.Item>

        <Space>
          <Button type="primary" htmlType="submit">
            Save and reload
          </Button>
          <Button onClick={onClose}>Cancel</Button>
        </Space>
      </Form>

      <Paragraph type="secondary" style={{ fontSize: 12, marginTop: 24 }}>
        The gateway never needs Kubernetes and the control plane never does either: cluster
        inventory is reported by the operator. That is enforced by the module graph, not by
        convention — see <Text code>core/</Text> and <Text code>operator/</Text>.
      </Paragraph>
    </Drawer>
  );
}
