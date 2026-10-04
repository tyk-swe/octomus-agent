import type { IconName } from './Icon.svelte';

export const NAVIGATION: {
  id: string;
  label: string;
  icon: IconName;
  heading: string;
  lede: string;
  noun?: string;
}[] = [
  {
    id: 'overview',
    label: 'Overview',
    icon: 'overview',
    heading: 'The bigger picture.',
    lede: 'A clear view of what’s happening, and what’s coming next.'
  },
  {
    id: 'queue',
    label: 'Task queue',
    icon: 'queue',
    heading: 'From idea to improvement.',
    lede: 'Every task has a purpose, a workspace, and a path to a reviewed PR.',
    noun: 'tasks'
  },
  {
    id: 'proposals',
    label: 'Proposals',
    icon: 'proposals',
    heading: 'Worth doing. Before doing.',
    lede: 'Grounded opportunities, challenged from two independent perspectives.',
    noun: 'proposals'
  },
  {
    id: 'prs',
    label: 'Pull requests',
    icon: 'prs',
    heading: 'Progress, ready for review.',
    lede: 'New improvements and continued work on your existing branches.',
    noun: 'pull requests'
  },
  {
    id: 'settings',
    label: 'Configuration',
    icon: 'settings',
    heading: 'Make it work your way.',
    lede: 'Your repository, your priorities, your operating limits.'
  }
];

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
