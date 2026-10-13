import { readable } from 'svelte/store';

// Retained records still age while their polling requests fail. Share one timer
// across timestamps, and stop it when no timestamp is mounted.
export const currentTime = readable(Date.now(), (set) => {
  set(Date.now());
  const timer = setInterval(() => set(Date.now()), 10000);
  return () => clearInterval(timer);
});
