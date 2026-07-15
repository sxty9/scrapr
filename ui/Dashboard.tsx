import {
  Badge,
  Panel,
  Stack,
  Text,
  useLiveQuery,
  userHasRight,
  type ServiceContextProps,
} from '@holistic/ui';
import type { Document, Info, Scraper } from './types';

// Backs permissions/scrapr.json → scrapr:use (and internal/rights.GroupUse on the backend).
// Admins always pass. Keep the three in sync.
const USE_RIGHT = 'hp_scrapr_use';

// scrapr is a headless agentic web scraper in M1: this panel is a read-only status view.
// Scrapers are created and triggered by consumers (studiq's Fuse tab) or the HTTP API.
export function Dashboard({ user, api }: ServiceContextProps) {
  const info = useLiveQuery<Info>(() => api.get<Info>('info'), 15000);
  const canUse = userHasRight(user, USE_RIGHT);

  return (
    <Stack gap={4}>
      <Panel title="Service" className="p-4">
        {info.data ? (
          <Stack gap={2}>
            <Stack direction="row" align="center" gap={2}>
              <Text weight="semibold">{info.data.service}</Text>
              <Badge variant="neutral">v{info.data.version}</Badge>
              {info.data.isAdmin && <Badge variant="accent">admin</Badge>}
            </Stack>
            <Text color="secondary">Signed in as {info.data.user}.</Text>
            <Text color="secondary">
              Agentic web scraper. Scrapers are managed by consumers (e.g. studiq) or the API.
            </Text>
          </Stack>
        ) : (
          <Text color={info.loading ? 'secondary' : 'danger'}>
            {info.loading ? 'Loading…' : 'Could not load service info.'}
          </Text>
        )}
      </Panel>

      {canUse ? (
        <StatusPanels api={api} />
      ) : (
        <Panel title="Scrapers" className="p-4">
          <Text color="secondary">
            You need the “Use scrapr” right to view scrapers and documents. An admin can grant it
            per user in the Rights (privleg) service.
          </Text>
        </Panel>
      )}
    </Stack>
  );
}

function StatusPanels({ api }: Pick<ServiceContextProps, 'api'>) {
  const scrapers = useLiveQuery<Scraper[]>(() => api.get<Scraper[]>('scrapers'), 15000);
  const docs = useLiveQuery<Document[]>(() => api.get<Document[]>('documents'), 15000);

  return (
    <Stack gap={4}>
      <Panel title={`Scrapers${scrapers.data ? ` (${scrapers.data.length})` : ''}`} className="p-4">
        {scrapers.data && scrapers.data.length > 0 ? (
          <Stack gap={2}>
            {scrapers.data.map((s) => (
              <Stack key={s.id} direction="row" align="center" gap={2}>
                <Text weight="semibold">{s.name}</Text>
                <Badge variant="neutral">{s.model}</Badge>
                {!s.enabled && <Badge variant="neutral">disabled</Badge>}
                {s.lastRun && (
                  <Badge variant={s.lastRun.status === 'ok' ? 'accent' : 'neutral'}>
                    {s.lastRun.status} · {s.lastRun.added}
                  </Badge>
                )}
              </Stack>
            ))}
          </Stack>
        ) : (
          <Text color="secondary">
            {scrapers.loading ? 'Loading…' : 'No scrapers yet — create one from a consumer or the API.'}
          </Text>
        )}
      </Panel>

      <Panel title={`Recent documents${docs.data ? ` (${docs.data.length})` : ''}`} className="p-4">
        {docs.data && docs.data.length > 0 ? (
          <Stack gap={1}>
            {docs.data.slice(0, 10).map((d) => (
              <Stack key={d.id} direction="row" align="center" gap={2}>
                <Text>{d.title}</Text>
                <Badge variant="neutral">{d.kategorie}</Badge>
              </Stack>
            ))}
          </Stack>
        ) : (
          <Text color="secondary">{docs.loading ? 'Loading…' : 'No documents scraped yet.'}</Text>
        )}
      </Panel>
    </Stack>
  );
}
