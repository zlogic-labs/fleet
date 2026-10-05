import { Alert } from 'antd';
import type { CSSProperties, ReactNode } from 'react';
import { ApiError, errorText } from '../api/client';

/**
 * Why the control plane's data is missing, said accurately.
 *
 * Four pages each had their own copy of "No control plane is answering", and
 * all four were shown for anything that stopped the data arriving. That merges
 * three different problems into one sentence: nothing is listening on that
 * port, the console has no admin token, and the server returned an error the
 * operator should read. The second is the one a reinstalled deployment hits
 * every time — the control plane is up, the gateway is using it, and the
 * console says it is not answering.
 */
export function ControlPlaneAlert({
  error,
  hint,
  style,
}: {
  error: unknown;
  hint: ReactNode;
  style?: CSSProperties;
}) {
  const status = error instanceof ApiError ? error.status : -1;

  if (status === 401 || status === 403) {
    return (
      <Alert
        showIcon
        type="warning"
        style={style}
        message="The control plane refused this console"
        description={
          <>
            It answered and rejected the credential. Paste the control-plane token into Settings —
            it is not the tenant API key, and the install script prints it. The server said:{' '}
            <code>{errorText(error)}</code>
          </>
        }
      />
    );
  }

  return (
    <Alert
      showIcon
      type="info"
      style={style}
      message="No control plane is answering"
      description={
        status === 0 || status === -1 ? (
          hint
        ) : (
          <>
            {hint} The server answered with an error: <code>{errorText(error)}</code>
          </>
        )
      }
    />
  );
}