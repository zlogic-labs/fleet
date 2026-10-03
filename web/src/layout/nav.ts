// The navigation tree, and the only place a new page has to be registered.
//
// A horizontal bar was the wrong shape: it holds about eight items before it
// wraps, and wrapping a primary navigation is how it stops being one. A tree
// grows by adding a leaf, and a group that outgrows its name becomes a level
// rather than a rewrite.
//
// Adding a page means adding one line here. Nothing else reads the route table
// except the router itself, and the router's paths are the keys below, so a
// typo shows up as a menu item that navigates nowhere rather than as a build
// error -- which is why App.tsx checks the two lists against each other at
// startup.

export interface NavItem {
  key: string;
  label: string;
}

export interface NavGroup {
  key: string;
  label: string;
  items: NavItem[];
}

export const NAV: NavGroup[] = [
  {
    key: 'serving',
    label: 'Serving',
    items: [
      { key: '/playground', label: 'Playground' },
      { key: '/fleet', label: 'Fleet' },
    ],
  },
  {
    key: 'infrastructure',
    label: 'Infrastructure',
    items: [
      { key: '/models', label: 'Models' },
      { key: '/cluster', label: 'Cluster' },
    ],
  },
  {
    key: 'billing',
    label: 'Tenancy & billing',
    items: [
      { key: '/tenancy', label: 'Tenants' },
      { key: '/cost', label: 'Cost' },
    ],
  },
];

// Settings sits outside the tree, at the foot of the sidebar: it configures the
// console rather than operating the fleet, and putting it in a group would
// imply it belongs to whichever group happened to be expanded.
export const SETTINGS: NavItem = { key: 'settings', label: 'Settings' };

export function groupOf(path: string): NavGroup | undefined {
  return NAV.find((g) => g.items.some((i) => i.key === path));
}

export function navPaths(): string[] {
  return NAV.flatMap((g) => g.items.map((i) => i.key));
}