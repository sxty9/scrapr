import { useState } from 'react';
import {
  Badge,
  Button,
  Field,
  Input,
  Panel,
  Stack,
  Text,
  useLiveQuery,
  userHasRight,
  type ServiceContextProps,
} from '@holistic/ui';
import type { Document, Info, Scraper, ScraperRun } from './types';

// Rights (mirror permissions/scrapr.json ⇄ internal/rights). Admins always pass.
const USE_RIGHT = 'hp_scrapr_use';
const RUN_RIGHT = 'hp_scrapr_run';

// scrapr's dashboard: run a crawl right here (URL → Scrape → documents), plus a live view of
// your scrapers and scraped documents. The agentic scraper is domain-agnostic; studiq is one
// consumer, but you can drive it standalone from this panel.
export function Dashboard({ user, api, ui }: ServiceContextProps) {
  const info = useLiveQuery<Info>(() => api.get<Info>('info'), 15000);
  const canUse = userHasRight(user, USE_RIGHT);
  const canRun = userHasRight(user, RUN_RIGHT);

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
            <Text color="secondary">
              Agentischer Web-Scraper — ein LLM entscheidet pro Seite, welchen Links es folgt und was es lädt.
            </Text>
          </Stack>
        ) : (
          <Text color={info.loading ? 'secondary' : 'danger'}>
            {info.loading ? 'Loading…' : 'Could not load service info.'}
          </Text>
        )}
      </Panel>

      {canRun && <ScrapeForm api={api} ui={ui} />}

      {canUse ? (
        <StatusPanels api={api} />
      ) : (
        <Panel title="Scrapers" className="p-4">
          <Text color="secondary">
            Du brauchst das Recht „Use scrapr", um Scraper und Dokumente zu sehen. Ein Admin kann es im
            Rechte-Service (privleg) pro Nutzer freischalten.
          </Text>
        </Panel>
      )}
    </Stack>
  );
}

// ScrapeForm creates a scraper for a URL and triggers it synchronously — the quickest way to
// see the agentic crawler work. The crawl runs on the server and can take up to ~90s.
function ScrapeForm({ api, ui }: Pick<ServiceContextProps, 'api' | 'ui'>) {
  const [url, setUrl] = useState('https://de.wikipedia.org/wiki/Graphentheorie');
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [run, setRun] = useState<ScraperRun | null>(null);

  async function scrape() {
    const source = url.trim();
    if (!source) {
      ui.toast({ title: 'Bitte eine URL eingeben', variant: 'error' });
      return;
    }
    setBusy(true);
    setRun(null);
    try {
      const scraper = await api.post<Scraper>('scrapers', {
        name: name.trim() || `Scrape — ${source}`,
        model: 'website',
        source,
        scheduleKind: 'manual',
        enabled: true,
      });
      const result = await api.post<ScraperRun>(`scrapers/${encodeURIComponent(scraper.id)}/trigger`);
      setRun(result);
      ui.toast({ title: `${result.added} Dokument(e) gescrapt`, variant: 'success' });
    } catch (e) {
      ui.toast({ title: 'Scrape fehlgeschlagen', description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Panel title="Neuer Scrape" className="p-4">
      <Stack gap={3}>
        <Field label="URL">
          <Input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://…" />
        </Field>
        <Field label="Name (optional)">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Mein Scraper" />
        </Field>
        <Stack direction="row" gap={2} align="center">
          <Button variant="primary" loading={busy} onClick={scrape}>
            Scrapen
          </Button>
          {busy && (
            <Text variant="footnote" color="secondary">
              Crawle… (läuft am Server, kann bis zu ~1 Minute dauern)
            </Text>
          )}
        </Stack>

        {run && (
          <Stack gap={1}>
            <Text weight="semibold">
              {run.added} Dokument(e){run.status !== 'ok' ? ` · ${run.status}` : ''}:
            </Text>
            {run.documents.map((d) => (
              <Stack key={d.id} direction="row" align="center" gap={2}>
                <Text>{d.title}</Text>
                <Badge variant="neutral">{d.kategorie}</Badge>
              </Stack>
            ))}
          </Stack>
        )}
      </Stack>
    </Panel>
  );
}

// StatusPanels is the live read-only view: all scrapers + the most recent documents.
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
          <Text color="secondary">{scrapers.loading ? 'Loading…' : 'Noch keine Scraper.'}</Text>
        )}
      </Panel>

      <Panel title={`Zuletzt gescrapt${docs.data ? ` (${docs.data.length})` : ''}`} className="p-4">
        {docs.data && docs.data.length > 0 ? (
          <Stack gap={1}>
            {docs.data.slice(0, 15).map((d) => (
              <Stack key={d.id} direction="row" align="center" gap={2}>
                <Text>{d.title}</Text>
                <Badge variant="neutral">{d.kategorie}</Badge>
              </Stack>
            ))}
          </Stack>
        ) : (
          <Text color="secondary">{docs.loading ? 'Loading…' : 'Noch keine Dokumente.'}</Text>
        )}
      </Panel>
    </Stack>
  );
}
