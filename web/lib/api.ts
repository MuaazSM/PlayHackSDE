export type Role = "STUDENT" | "MANAGER" | "SECRETARY" | string;

export type Session = {
  token: string;
  userId: string;
  rollNo: string;
  role: Role;
};

export type Facility = {
  id: string;
  name: string;
  sport: string;
  is_exclusive: boolean;
  capacity: number;
  opens_at: string;
  closes_at: string;
  slot_minutes: number;
  min_duration_minutes: number;
  max_duration_minutes: number;
};

export type GridSlot = { start: string; end: string };
export type GridFacility = Pick<Facility, "id" | "name" | "sport" | "is_exclusive">;
export type CampusGrid = {
  date: string;
  facilities: GridFacility[];
  slots: GridSlot[];
  grid: string[][];
};

export type DaySlot = {
  start: string;
  end: string;
  state: string;
  remaining?: number;
  capacity?: number;
};
export type DayAvailability = { facility_id: string; date: string; slots: DaySlot[] };

export type Booking = {
  id: string;
  reference: string;
  facility_id: string;
  facility?: string;
  start: string;
  end: string;
  status: string;
};
export type BookingList = { upcoming: Booking[]; past: Booking[] };

export type WaitlistEntry = {
  id: string;
  facility_id: string;
  start: string;
  end: string;
  position: number;
  status: string;
};

export type AffectedBooking = {
  booking_id: string;
  user_id: string;
  roll_no: string;
  name: string;
  start: string;
  end: string;
  status: string;
};
export type Closure = {
  id: string;
  facility_id: string;
  facility?: string;
  start: string;
  end: string;
  status: string;
  reason?: string;
  created_at: string;
  slots_closed?: number;
  affected_bookings: AffectedBooking[];
};

export type ErrorBody = {
  error: string;
  message: string;
  alternatives?: { facility_id: string; name: string; start: string }[];
  waitlist_available?: boolean;
  conflicts?: { booking_id: string; roll_no: string; name: string; start: string; end: string }[];
  limit?: string;
  resets_at?: string;
  request_id?: string;
};

export type AnalyticsReport = {
  from: string;
  to: string;
  utilisation: {
    facility_id: string;
    facility_name: string;
    hour: number;
    booked_hours: number;
    available_hours: number;
    utilisation: number;
  }[];
  peak_demand: {
    weekdays: string[];
    hours: number[];
    cells: number[][];
    peak: { weekday: number; hour: number; count: number };
  };
  no_show: { facility_id: string; facility_name: string; total: number; no_shows: number; rate: number }[];
  unmet_demand: { facility_id: string; facility_name: string; hour: number; entries: number }[];
  slot_recovery: { promoted: number; recovered: number; rate: number };
};

export type CheckinToken = {
  facility_id: string;
  token: string;
  refresh_in_seconds: number;
  valid_for_seconds: number;
  issued_at: string;
};

export type RaceResult = {
  n: number;
  confirmed: number;
  conflict_409: number;
  other: number;
  db_count: number;
  elapsed_ms: number;
  p50_ms: number;
  p99_ms: number;
  reject_p99_ms: number;
  start_spread_ms: number;
  winner?: { booking_id: string; reference: string; user: string };
  errors?: string[];
};

export type ResetResult = {
  facility_id: string;
  start: string;
  end: string;
  cancelled: number;
  db_count: number;
};

export class ApiError extends Error {
  status: number;
  code: string;
  body: ErrorBody;
  retryAfter?: string;

  constructor(status: number, body: ErrorBody, retryAfter?: string) {
    super(body.message || "Request failed");
    this.name = "ApiError";
    this.status = status;
    this.code = body.error || "INTERNAL";
    this.body = body;
    this.retryAfter = retryAfter;
  }
}

const API_BASE = (process.env.NEXT_PUBLIC_API_BASE_URL || "http://localhost:8080").replace(/\/$/, "");

export function apiBaseUrl() {
  return API_BASE;
}

export function makeIdempotencyKey() {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) return crypto.randomUUID();
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

async function request<T>(path: string, options: RequestInit = {}, token?: string): Promise<T> {
  const headers = new Headers(options.headers);
  headers.set("Accept", "application/json");
  if (options.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  if (token) headers.set("Authorization", `Bearer ${token}`);

  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, { ...options, headers, cache: "no-store" });
  } catch {
    throw new ApiError(0, { error: "NETWORK", message: "The API is unreachable. Is the local server running?" });
  }
  const text = await response.text();
  let data: unknown = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!response.ok) {
    const body = (data && typeof data === "object" ? data : {}) as ErrorBody;
    throw new ApiError(response.status, body, response.headers.get("Retry-After") || undefined);
  }
  return data as T;
}

export const api = {
  login: (rollNo: string) => request<{ token: string; user_id: string; roll_no: string; role: Role }>("/api/v1/dev/login", {
    method: "POST",
    body: JSON.stringify({ roll_no: rollNo }),
  }),
  facilities: (token: string) => request<{ facilities: Facility[] }>("/api/v1/facilities", {}, token),
  campusAvailability: (date: string, token: string) => request<CampusGrid>(`/api/v1/availability?date=${encodeURIComponent(date)}`, {}, token),
  facilityAvailability: (id: string, date: string, token: string) => request<DayAvailability>(`/api/v1/facilities/${id}/availability?date=${encodeURIComponent(date)}`, {}, token),
  bookings: (token: string) => request<BookingList>("/api/v1/bookings/me", {}, token),
  createBooking: (body: { facility_id: string; start: string; duration_minutes: number }, token: string) => request<Booking>("/api/v1/bookings", {
    method: "POST", headers: { "Idempotency-Key": makeIdempotencyKey() }, body: JSON.stringify(body),
  }, token),
  cancelBooking: (id: string, token: string) => request<Booking>(`/api/v1/bookings/${id}`, { method: "DELETE" }, token),
  claimBooking: (id: string, token: string) => request<Booking>(`/api/v1/bookings/${id}/claim`, { method: "POST" }, token),
  joinWaitlist: (body: { facility_id: string; start: string; duration_minutes: number }, token: string) => request<WaitlistEntry>("/api/v1/waitlist", { method: "POST", body: JSON.stringify(body) }, token),
  leaveWaitlist: (id: string, token: string) => request<{ id: string; status: string }>(`/api/v1/waitlist/${id}`, { method: "DELETE" }, token),
  checkIn: (id: string, checkinToken: string, token: string) => request<{ booking_id: string; reference: string; facility_id: string; start: string; end: string; checked_in_at: string; method: string }>(`/api/v1/bookings/${id}/check-in`, { method: "POST", body: JSON.stringify({ token: checkinToken }) }, token),
  closures: (token: string, date?: string) => request<{ closures: Closure[] }>(`/api/v1/closures${date ? `?date=${encodeURIComponent(date)}` : ""}`, {}, token),
  createClosure: (body: { facility_id: string; start: string; end: string; reason: string }, token: string) => request<Closure>("/api/v1/closures", { method: "POST", headers: { "Idempotency-Key": makeIdempotencyKey() }, body: JSON.stringify(body) }, token),
  reopenClosure: (id: string, token: string) => request<Closure>(`/api/v1/closures/${id}`, { method: "DELETE" }, token),
  analytics: (from: string, to: string, token: string) => request<AnalyticsReport>(`/api/v1/admin/analytics?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`, {}, token),
  checkinToken: (id: string, token: string) => request<CheckinToken>(`/api/v1/facilities/${id}/checkin-token`, {}, token),
  race: (body: { facility_id: string; start: string; duration_minutes: number; n: number }, token: string) => request<RaceResult>("/api/v1/demo/race", { method: "POST", body: JSON.stringify(body) }, token),
  resetRace: (body: { facility_id: string; start: string; duration_minutes: number }, token: string) => request<ResetResult>("/api/v1/demo/reset", { method: "POST", body: JSON.stringify(body) }, token),
};
