import type { IconName } from './Icon.svelte';

export const NAVIGATION: {
  id: string;
  label: string;
  icon: IconName;
  noun?: string;
}[] = [
  {
    id: 'overview',
    label: 'Overview',
    icon: 'overview'
  },
  {
    id: 'queue',
    label: 'Task queue',
    icon: 'queue',
    noun: 'tasks'
  },
  {
    id: 'proposals',
    label: 'Proposals',
    icon: 'proposals',
    noun: 'proposals'
  },
  {
    id: 'requests',
    label: 'Requests',
    icon: 'inbox'
  },
  {
    id: 'prs',
    label: 'Pull requests',
    icon: 'prs',
    noun: 'pull requests'
  },
  {
    id: 'insights',
    label: 'Insights',
    icon: 'chart'
  },
  {
    id: 'settings',
    label: 'Configuration',
    icon: 'settings'
  }
];

export function viewFromHash(hash: string): string | null {
  const id = hash.startsWith('#') ? hash.slice(1) : hash;
  if (id === '') return 'overview';
  return NAVIGATION.some((item) => item.id === id) ? id : null;
}

export const TOGGLE_PENDING_LABELS: Record<string, string> = {
  resume: 'Starting continuous…',
  pause: 'Pausing…'
};

export const QUEUE_FILTERS = [
  'all',
  'active',
  'queued',
  'published',
  'attention',
  'blocked',
  'cancelled'
];

export const PR_FILTERS = ['all', 'open', 'merged', 'closed'];
