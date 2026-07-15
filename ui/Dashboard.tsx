import { useState } from 'react';
import {
  Badge,
  Button,
  CodeBlock,
  Field,
  Input,
  Markdown,
  Modal,
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

type Content =
  | { kind: 'markdown'; text: string }
  | { kind: 'text'; text: string; ct: string }
  | { kind: 'image'; url: string; ct: string }
  | { kind: 'binary'; url: string; ct: string; size: number }
  | { kind: 'none' };

// scrapr's dashboard: run a crawl (URL → Scrape → documents), browse your scrapers/documents,
// and click any document to view its stored content (fetched from lakearch).
export function Dashboard({ user, api, ui }: ServiceContextProps) {
  const info = useLiveQuery<Info>(() => api.get<Info>('info'), 15000);
  const canUse = userHasRight(user, USE_RIGHT);
  const canRun = userHasRight(user, RUN_RIGHT);

  const [viewing, setViewing] = useState<Document | null>(null);
  const [content, setContent] = useState<Content | null>(null);
  const [loadingC, setLoadingC] = useState(false);

  function revoke(c: Content | null) {
    if (c && (c.kind === 'image' || c.kind === 'binary')) URL.revokeObjectURL(c.url);
  }
  function closeDoc() {
    revoke(content);
    setContent(null);
    setViewing(null);
  }
  async function openDoc(d: Document) {
    revoke(content);
    setViewing(d);
    setContent(null);
    setLoadingC(true);
    try {
      const res = await api.raw(`documents/${encodeURIComponent(d.id)}/content`);
      if (res.status === 404) {
        setContent({ kind: 'none' });
        return;
      }
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const ct = (res.headers.get('content-type') || '').toLowerCase();
      if (ct.includes('markdown')) {
        setContent({ kind: 'markdown', text: await res.text() });
      } else if (ct.startsWith('image/')) {
        setContent({ kind: 'image', url: URL.createObjectURL(await res.blob()), ct });
      } else if (ct.startsWith('text/') || ct.includes('json') || ct.includes('xml')) {
        setContent({ kind: 'text', text: await res.text(), ct });
      } else {
        const blob = await res.blob();
        setContent({ kind: 'binary', url: URL.createObjectURL(blob), ct, size: blob.size });
      }
    } catch (e) {
      ui.toast({ title: 'Inhalt konnte nicht geladen werden', description: (e as Error).message, variant: 'error' });
      setViewing(null);
    } finally {
      setLoadingC(false);
    }
  }

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

      {canRun && <ScrapeForm api={api} ui={ui} onOpen={openDoc} />}

      {canUse ? (
        <StatusPanels api={api} onOpen={openDoc} />
      ) : (
        <Panel title="Scrapers" className="p-4">
          <Text color="secondary">
            Du brauchst das Recht „Use scrapr", um Scraper und Dokumente zu sehen. Ein Admin kann es im
            Rechte-Service (privleg) pro Nutzer freischalten.
          </Text>
        </Panel>
      )}

      <Modal
        open={!!viewing}
        onOpenChange={(o) => {
          if (!o) closeDoc();
        }}
        title={viewing?.title ?? 'Dokument'}
        description={viewing?.kategorie}
        size="lg"
      >
        <DocContent content={content} loading={loadingC} />
      </Modal>
    </Stack>
  );
}

// DocRow renders a document as a clickable row (title opens the viewer) + its kategorie badge.
function DocRow({ d, onOpen }: { d: Document; onOpen: (d: Document) => void }) {
  return (
    <Stack direction="row" align="center" gap={2}>
      <Button variant="ghost" size="sm" onClick={() => onOpen(d)}>
        {d.title}
      </Button>
      {d.kategorie && <Badge variant="neutral">{d.kategorie}</Badge>}
    </Stack>
  );
}

// DocContent renders the selected document's stored content (from lakearch) inside the modal:
// markdown rendered, text in a code block, images/binaries opened in a new browser tab.
function DocContent({ content, loading }: { content: Content | null; loading: boolean }) {
  if (loading) return <Text color="secondary">Lade Inhalt…</Text>;
  if (!content) return null;
  switch (content.kind) {
    case 'markdown':
      return <Markdown text={content.text} />;
    case 'text':
      return <CodeBlock code={content.text} />;
    case 'image':
      return (
        <Stack gap={2}>
          <Text color="secondary">Bild ({content.ct})</Text>
          <Stack direction="row">
            <Button variant="secondary" size="sm" onClick={() => window.open(content.url, '_blank')}>
              In neuem Tab öffnen
            </Button>
          </Stack>
        </Stack>
      );
    case 'binary':
      return (
        <Stack gap={2}>
          <Text color="secondary">
            Binärdatei ({content.ct}, {Math.round(content.size / 1024)} KB)
          </Text>
          <Stack direction="row">
            <Button variant="secondary" size="sm" onClick={() => window.open(content.url, '_blank')}>
              Öffnen / Herunterladen
            </Button>
          </Stack>
        </Stack>
      );
    case 'none':
      return <Text color="secondary">Kein Inhalt gespeichert (nur Metadaten).</Text>;
    default:
      return null;
  }
}

// ScrapeForm creates a scraper for a URL and triggers it synchronously — the quickest way to
// see the agentic crawler work. The crawl runs on the server (~45–60s on a warm model).
function ScrapeForm({ api, ui, onOpen }: Pick<ServiceContextProps, 'api' | 'ui'> & { onOpen: (d: Document) => void }) {
  const [url, setUrl] = useState('https://de.wikipedia.org/wiki/Graphentheorie');
  const [name, setName] = useState('');
  const [cats, setCats] = useState('');
  const [busy, setBusy] = useState(false);
  const [run, setRun] = useState<ScraperRun | null>(null);

  async function scrape() {
    const source = url.trim();
    if (!source) {
      ui.toast({ title: 'Bitte eine URL eingeben', variant: 'error' });
      return;
    }
    const categories = cats
      .split(',')
      .map((c) => c.trim())
      .filter(Boolean);
    setBusy(true);
    setRun(null);
    try {
      const scraper = await api.post<Scraper>('scrapers', {
        name: name.trim() || `Scrape — ${source}`,
        model: 'website',
        source,
        scheduleKind: 'manual',
        enabled: true,
        categories,
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
        <Field label="Kategorien (optional, kommagetrennt)" hint="Leer = das LLM vergibt freie Labels. Sonst klassifiziert es nur in diese.">
          <Input value={cats} onChange={(e) => setCats(e.target.value)} placeholder="z. B. Artikel, Bild, Quelle" />
        </Field>
        <Stack direction="row" gap={2} align="center">
          <Button variant="primary" loading={busy} onClick={scrape}>
            Scrapen
          </Button>
          {busy && (
            <Text variant="footnote" color="secondary">
              Crawle… (läuft am Server, meist ~45–60 s)
            </Text>
          )}
        </Stack>

        {run && (
          <Stack gap={1}>
            <Text weight="semibold">
              {run.added} Dokument(e){run.status !== 'ok' ? ` · ${run.status}` : ''} — zum Ansehen anklicken:
            </Text>
            {run.documents.map((d) => (
              <DocRow key={d.id} d={d} onOpen={onOpen} />
            ))}
          </Stack>
        )}
      </Stack>
    </Panel>
  );
}

// StatusPanels is the live read-only view: all scrapers + the most recent documents.
function StatusPanels({ api, onOpen }: Pick<ServiceContextProps, 'api'> & { onOpen: (d: Document) => void }) {
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
            {docs.data.slice(0, 20).map((d) => (
              <DocRow key={d.id} d={d} onOpen={onOpen} />
            ))}
          </Stack>
        ) : (
          <Text color="secondary">{docs.loading ? 'Loading…' : 'Noch keine Dokumente.'}</Text>
        )}
      </Panel>
    </Stack>
  );
}
