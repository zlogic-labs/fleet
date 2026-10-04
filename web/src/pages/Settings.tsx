import { Alert, Button, Drawer, Form, Input, Space, Typography } from 'antd';

import {
  apiKey,
  controlToken,
  origins,
  setApiKey,
  setControlToken,
  setOrigin,
} from '../api/client';

const { Paragraph, Text } = Typography;

/**
 * Where the gateway and the control plane live, and the credential for each.
 *
 * They are separate settings because they are separate processes: the gateway
 * is the data plane and the control plane is the management API. In the common
 * case the console is served by the gateway itself and the control plane is on
 * 8081, but an operator running them apart needs to say so.
 *
 * The two credentials are separate for the same reason, and it is not a
 * formality: the tenant key may spend money, and the control plane's token may
 * set the price, issue keys and close a billing period. One field for both would
 * mean either over-granting the data plane or under-granting the control plane.
 */
interface SettingsValues {
  gateway: string;
  control: string;
  apiKey?: string;
  controlToken?: string;
}

export function Settings({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [form] = Form.useForm<SettingsValues>();

  const save = async (values: SettingsValues) => {
    setOrigin('gateway', values.gateway);
    setOrigin('control', values.control);
    setApiKey(values.apiKey ?? '');
    setControlToken(values.controlToken ?? '');
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
          controlToken: controlToken(),
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

        <Form.Item name="apiKey" label="API key" extra="Sent as a bearer token to the gateway, to spend as a tenant.">
          <Input.Password placeholder="sk-..." autoComplete="off" />
        </Form.Item>

        <Form.Item
          name="controlToken"
          label="Control plane token"
          extra="FLEET_ADMIN_TOKEN. Needed unless the control plane is bound to loopback."
        >
          <Input.Password placeholder="operator token" autoComplete="off" />
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
