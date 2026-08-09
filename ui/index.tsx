import { BoltIcon, type ServicePlugin } from '@holisdk/ui';
import { Dashboard } from './Dashboard';

// scrapr's dashboard plugin. Linked into holistic/frontend/external/<id> at install time and
// discovered by the host SPA's build-time registry. `id` MUST equal the link dir name and the
// permissions manifest's "service" field. In M1 scrapr is headless — this is a read-only status
// panel; scrapers are created/run by consumers (studiq's Fuse tab) or the API.
const plugin: ServicePlugin = {
  id: 'scrapr',
  displayName: 'Scrapr',
  icon: BoltIcon,
  order: 100,
  Component: Dashboard,
};

export default plugin;
