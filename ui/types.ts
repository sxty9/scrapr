// Shapes returned by the backend under /api/services/scrapr/.

export interface Info {
  service: string;
  version: string;
  user: string;
  isAdmin: boolean;
  kinds?: string[];
}

export interface Scraper {
  id: string;
  name: string;
  model: string;
  source: string;
  scheduleKind: string;
  enabled: boolean;
  lastRun?: { at: string; status: string; added: number };
}

export interface Document {
  id: string;
  title: string;
  kategorie: string;
  source: string;
  addedAt: string;
}
