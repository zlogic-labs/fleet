/** Calls against the control plane (fleet-apiserver). */

import { request } from './client';
import type { ClusterStatus, Deployment, PullJob, RegistryModel, StorageInfo } from '../types';

const BASE = '/api/v1';

export const models = {
  list: (signal?: AbortSignal) => request<RegistryModel[]>(`${BASE}/models`, { signal }),
  get: (name: string, signal?: AbortSignal) =>
    request<RegistryModel>(`${BASE}/models/${encodeURIComponent(name)}`, { signal }),
  remove: (name: string) =>
    request<void>(`${BASE}/models/${encodeURIComponent(name)}`, { method: 'DELETE' }),
};

export interface PullRequest {
  model: string;
  source: 'huggingface' | 'url';
  sourceRef: string;
  revision?: string;
  tokenizerId?: string;
  contextLimit?: number;
  hfToken?: string;
}

export const pulls = {
  start: (body: PullRequest) => request<PullJob>(`${BASE}/pulls`, { method: 'POST', body }),
  list: (signal?: AbortSignal) => request<PullJob[]>(`${BASE}/pulls`, { signal }),
  get: (id: string, signal?: AbortSignal) => request<PullJob>(`${BASE}/pulls/${id}`, { signal }),
  cancel: (id: string) => request<void>(`${BASE}/pulls/${id}`, { method: 'DELETE' }),
};

export const storage = {
  get: (signal?: AbortSignal) => request<StorageInfo>(`${BASE}/storage`, { signal }),
};

export const cluster = {
  get: (signal?: AbortSignal) => request<ClusterStatus>(`${BASE}/cluster`, { signal }),
};

export const deployments = {
  list: (signal?: AbortSignal) => request<Deployment[]>(`${BASE}/deployments`, { signal }),
  scale: (name: string, replicas: number) =>
    request<void>(`${BASE}/deployments/${encodeURIComponent(name)}/scale`, {
      method: 'POST',
      body: { replicas },
    }),
  remove: (name: string) =>
    request<void>(`${BASE}/deployments/${encodeURIComponent(name)}`, { method: 'DELETE' }),
};
