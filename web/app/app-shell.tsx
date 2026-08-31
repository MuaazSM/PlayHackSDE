"use client";

import {
  Activity,
  ArrowLeft,
  BarChart3,
  CalendarDays,
  Check,
  CheckCircle2,
  ChevronRight,
  Clock3,
  Flame,
  LogOut,
  Menu,
  RefreshCw,
  ShieldCheck,
  Signal,
  Ticket,
  Trophy,
  Users,
  X,
} from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { api, apiBaseUrl, ApiError, type AnalyticsReport, type Booking, type CampusGrid, type Closure, type Facility, type RaceResult, type ResetResult, type Session, type WaitlistEntry } from "../lib/api";

type View = "discover" | "bookings" | "checkin" | "manager" | "race";
type SlotChoice = { facility: Facility; start: string; end: string; state: string; remaining?: number; capacity?: number };

const SESSION_KEY = "playhack.session";
const CAMPUS_TIME_ZONE = "Asia/Kolkata";
const todayISO = () => {
  const parts = new Intl.DateTimeFormat("en-US", { timeZone: CAMPUS_TIME_ZONE, year: "numeric", month: "2-digit", day: "2-digit" }).formatToParts(new Date());
  const get = (type: string) => parts.find((part) => part.type === type)?.value || "01";
  return `${get("year")}-${get("month")}-${get("day")}`;
};

function readSession(): Session | null {
  try {
    const raw = localStorage.getItem(SESSION_KEY);
    return raw ? (JSON.parse(raw) as Session) : null;
  } catch {
    return null;
  }
}

function displayTime(value: string) {
  return new Intl.DateTimeFormat("en-IN", { timeZone: CAMPUS_TIME_ZONE, hour: "numeric", minute: "2-digit" }).format(new Date(value));
}

function displayDate(value: string) {
  return new Intl.DateTimeFormat("en-IN", { timeZone: CAMPUS_TIME_ZONE, weekday: "short", day: "numeric", month: "short" }).format(new Date(value));
}

function toDateTimeLocal(value: string) {
  const d = new Date(value);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function toRFC3339(value: string) {
  // datetime-local is a campus wall-clock input. Keep the explicit IST offset
  // so a presenter running the browser in another timezone cannot shift a slot.
  return `${value.length === 16 ? `${value}:00` : value}+05:30`;
}

function friendlyError(error: unknown) {
  if (error instanceof ApiError) {
    if (error.code === "NETWORK") return error.message;
    if (error.code === "SERVICE_BUSY" || error.code === "RATE_LIMITED") return `${error.message}${error.retryAfter ? ` Retry in ${error.retryAfter}s.` : ""}`;
    return error.message || "The request could not be completed.";
  }
  return "The request could not be completed. Try again.";
}

function StatusBadge({ state }: { state: string }) {
  const label = state.replaceAll("_", " ").toLowerCase();
  return <span className={`badge badge-${state.toLowerCase()}`}>{label}</span>;
}

function Loading({ label = "Loading" }: { label?: string }) {
  return <div className="loading"><RefreshCw size={16} className="spin" aria-hidden="true" /> {label}</div>;
}

function Empty({ icon: Icon, title, body }: { icon: typeof CalendarDays; title: string; body: string }) {
  return <div className="empty"><Icon size={28} aria-hidden="true" /><strong>{title}</strong><p>{body}</p></div>;
}

export default function AppShell({ initialView = "discover" }: { initialView?: View }) {
  const [hydrated, setHydrated] = useState(false);
  const [session, setSession] = useState<Session | null>(null);
  const [view, setView] = useState<View>(initialView);

  useEffect(() => {
    setSession(readSession());
    setHydrated(true);
  }, []);

  const login = (next: Session) => {
    localStorage.setItem(SESSION_KEY, JSON.stringify(next));
    setSession(next);
  };
  const logout = () => {
    localStorage.removeItem(SESSION_KEY);
    setSession(null);
    setView("discover");
  };

  if (!hydrated) return <div className="page-loading"><Loading label="Preparing PlayHack" /></div>;
  if (!session) return <LoginScreen onLogin={login} />;
  return <Workspace session={session} view={view} setView={setView} onLogout={logout} />;
}

function LoginScreen({ onLogin }: { onLogin: (session: Session) => void }) {
  const [rollNo, setRollNo] = useState("student01");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [showRoles, setShowRoles] = useState(false);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const result = await api.login(rollNo.trim());
      onLogin({ token: result.token, userId: result.user_id, rollNo: result.roll_no, role: result.role });
    } catch (err) {
      setError(friendlyError(err));
    } finally {
      setBusy(false);
    }
  };

  return <main className="login-shell">
    <section className="login-art" aria-label="PlayHack booking overview">
      <div className="brand brand-light"><span className="brand-mark"><Ticket size={18} /></span> PlayHack</div>
      <div className="login-art-copy"><p className="eyebrow">IIT Guwahati sports board</p><h1>Make time for the game.</h1><p>One campus grid. One clear winner. Reserve courts and shared spaces without the guesswork.</p></div>
      <div className="login-art-stat"><strong>7</strong><span>facilities in one view</span><strong>24/7</strong><span>availability clarity</span></div>
    </section>
    <section className="login-panel">
      <div className="brand"><span className="brand-mark"><Ticket size={18} /></span> PlayHack</div>
      <div className="login-heading"><p className="eyebrow">Welcome back</p><h2>Sign in to book</h2><p>Use your campus roll number to continue.</p></div>
      <form onSubmit={submit} className="stack-form">
        <label htmlFor="roll-no">Roll number</label>
        <input id="roll-no" value={rollNo} onChange={(e) => setRollNo(e.target.value)} placeholder="student01" autoComplete="username" required />
        {error && <div className="inline-error" role="alert"><X size={16} />{error}</div>}
        <button className="primary-button" type="submit" disabled={busy || !rollNo.trim()}>{busy ? <><RefreshCw size={16} className="spin" /> Signing in</> : <>Continue <ChevronRight size={16} /></>}</button>
      </form>
      <button className="text-button" type="button" onClick={() => setShowRoles((open) => !open)}>Need a demo account? {showRoles ? "Hide" : "Show options"}</button>
      {showRoles && <div className="demo-accounts"><button type="button" onClick={() => setRollNo("student01")}>Student <span>student01</span></button><button type="button" onClick={() => setRollNo("manager01")}>Manager <span>manager01</span></button><button type="button" onClick={() => setRollNo("secretary01")}>Secretary <span>secretary01</span></button></div>}
      <p className="login-footnote">Development sign-in is enabled by the API. Production deployments should connect this screen to institute OIDC.</p>
    </section>
  </main>;
}

function Workspace({ session, view, setView, onLogout }: { session: Session; view: View; setView: (view: View) => void; onLogout: () => void }) {
  const [mobileNav, setMobileNav] = useState(false);
  const [facilities, setFacilities] = useState<Facility[]>([]);
  const [grid, setGrid] = useState<CampusGrid | null>(null);
  const [date, setDate] = useState(todayISO());
  const [loadingFacilities, setLoadingFacilities] = useState(true);
  const [loadingGrid, setLoadingGrid] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [refreshTick, setRefreshTick] = useState(0);
  const [toast, setToast] = useState("");
  const [selectedSlot, setSelectedSlot] = useState<SlotChoice | null>(null);
  const [waitlistEntries, setWaitlistEntries] = useState<WaitlistEntry[]>([]);

  useEffect(() => {
    let active = true;
    setLoadingFacilities(true);
    api.facilities(session.token).then((result) => { if (active) setFacilities(result.facilities || []); }).catch((err) => { if (active) setLoadError(friendlyError(err)); }).finally(() => { if (active) setLoadingFacilities(false); });
    return () => { active = false; };
  }, [session.token]);

  useEffect(() => {
    let active = true;
    setLoadingGrid(true);
    api.campusAvailability(date, session.token).then((result) => { if (active) { setGrid(result); setLoadError(""); } }).catch((err) => { if (active) setLoadError(friendlyError(err)); }).finally(() => { if (active) setLoadingGrid(false); });
    return () => { active = false; };
  }, [date, session.token, refreshTick]);

  useEffect(() => {
    if (!toast) return;
    const timer = window.setTimeout(() => setToast(""), 5000);
    return () => window.clearTimeout(timer);
  }, [toast]);

  useEffect(() => {
    const source = new EventSource(`${apiBaseUrl()}/api/v1/stream?date=${encodeURIComponent(date)}&access_token=${encodeURIComponent(session.token)}`);
    const refresh = () => setRefreshTick((tick) => tick + 1);
    source.addEventListener("slot", refresh);
    return () => { source.removeEventListener("slot", refresh); source.close(); };
  }, [date, session.token]);

  const navItems: { id: View; label: string; icon: typeof CalendarDays; roles?: string[] }[] = [
    { id: "discover", label: "Discover", icon: CalendarDays },
    { id: "bookings", label: "My bookings", icon: Ticket },
    { id: "checkin", label: "Check-in", icon: CheckCircle2 },
    ...(session.role === "MANAGER" || session.role === "SECRETARY" ? [{ id: "manager" as View, label: "Manager console", icon: BarChart3 }] : []),
    { id: "race", label: "Race proof", icon: Trophy },
  ];

  const chooseView = (next: View) => { setView(next); setMobileNav(false); };
  return <div className="app-frame">
    <aside className={`sidebar ${mobileNav ? "open" : ""}`}>
      <div className="sidebar-top"><div className="brand"><span className="brand-mark"><Ticket size={17} /></span> PlayHack</div><button className="icon-button sidebar-close" type="button" onClick={() => setMobileNav(false)} aria-label="Close menu"><X size={18} /></button></div>
      <div className="campus-chip"><span className="status-dot" /> IIT Guwahati <span className="chip-live">LIVE</span></div>
      <nav aria-label="Main navigation">{navItems.map(({ id, label, icon: Icon }) => <button key={id} type="button" className={`nav-item ${view === id ? "active" : ""}`} onClick={() => chooseView(id)}><Icon size={17} /> {label}{id === "manager" && <span className="nav-role">{session.role.toLowerCase()}</span>}</button>)}</nav>
      <div className="sidebar-bottom"><div className="account"><span className="avatar">{session.rollNo.slice(-2)}</span><div><strong>{session.rollNo}</strong><small>{session.role.toLowerCase()}</small></div><button className="icon-button" type="button" onClick={onLogout} aria-label="Sign out" title="Sign out"><LogOut size={16} /></button></div><p className="sidebar-note">Availability is derived from confirmed bookings. A stale screen can never create a double booking.</p></div>
    </aside>
    {mobileNav && <button className="scrim" type="button" aria-label="Close navigation" onClick={() => setMobileNav(false)} />}
    <main className="main-column">
      <header className="topbar"><button className="icon-button menu-button" type="button" onClick={() => setMobileNav(true)} aria-label="Open menu"><Menu size={20} /></button><div className="crumb"><span>Sports booking</span><ChevronRight size={14} /><strong>{navItems.find((item) => item.id === view)?.label}</strong></div><div className="topbar-actions"><span className="connection"><Signal size={15} /> API connected</span><span className="date-label">{displayDate(`${date}T12:00:00`)}</span></div></header>
      <div className="content">
        {view === "discover" && <Discover grid={grid} facilities={facilities} date={date} setDate={setDate} loading={loadingGrid || loadingFacilities} error={loadError} onRefresh={() => setRefreshTick((tick) => tick + 1)} onPick={(choice) => setSelectedSlot(choice)} />}
        {view === "bookings" && <BookingsView session={session} facilities={facilities} waitlistEntries={waitlistEntries} onToast={setToast} onRefresh={() => setRefreshTick((tick) => tick + 1)} onRemoveWaitlist={(id) => setWaitlistEntries((current) => current.filter((entry) => entry.id !== id))} />}
        {view === "checkin" && <CheckinView session={session} onToast={setToast} />}
        {view === "manager" && <ManagerView session={session} facilities={facilities} onToast={setToast} />}
        {view === "race" && <RaceView session={session} facilities={facilities} />}
      </div>
    </main>
    {toast && <div className="toast" role="status"><CheckCircle2 size={17} /> {toast}<button className="toast-close" type="button" onClick={() => setToast("")} aria-label="Dismiss notification"><X size={14} /></button></div>}
    {selectedSlot && <BookingModal choice={selectedSlot} session={session} onClose={() => setSelectedSlot(null)} onToast={setToast} onRefresh={() => setRefreshTick((tick) => tick + 1)} onWaitlist={(entry) => { setWaitlistEntries((current) => [...current, entry]); }} />}
  </div>;
}

function Discover({ grid, facilities, date, setDate, loading, error, onRefresh, onPick }: { grid: CampusGrid | null; facilities: Facility[]; date: string; setDate: (date: string) => void; loading: boolean; error: string; onRefresh: () => void; onPick: (choice: SlotChoice) => void }) {
  const [sport, setSport] = useState("all");
  const sportOptions = ["all", ...Array.from(new Set(facilities.map((facility) => facility.sport)))];
  const visible = grid?.facilities.filter((facility) => sport === "all" || facility.sport === sport) || [];
  const byId = new Map(facilities.map((facility) => [facility.id, facility]));
  return <section className="view-stack">
    <div className="page-heading"><div><p className="eyebrow">The campus, at a glance</p><h1>Find your next game.</h1><p className="lede">Choose a facility and a slot. The confirmation is decided by the database, not by a stale availability check.</p></div><div className="heading-actions"><button className="secondary-button" type="button" onClick={onRefresh} disabled={loading}><RefreshCw size={16} className={loading ? "spin" : ""} /> Refresh</button></div></div>
    <div className="toolbar"><label className="date-control"><CalendarDays size={16} /><span>Date</span><input type="date" value={date} onChange={(e) => setDate(e.target.value)} /></label><div className="segmented" role="group" aria-label="Filter by sport">{sportOptions.map((value) => <button key={value} className={sport === value ? "selected" : ""} type="button" onClick={() => setSport(value)}>{value === "all" ? "All sports" : value}</button>)}</div><span className="live-hint"><span className="status-dot" /> Live updates on</span></div>
    {error && <div className="error-panel" role="alert"><Activity size={19} /><div><strong>We could not load the campus grid.</strong><p>{error}</p></div><button className="secondary-button" type="button" onClick={onRefresh}>Try again</button></div>}
    {loading && !grid ? <div className="panel"><Loading label="Loading facilities and availability" /></div> : !grid || !grid.facilities.length ? <Empty icon={CalendarDays} title="No facilities found" body="Seed the API catalogue, then refresh this view." /> : <div className="availability-panel"><div className="panel-header"><div><h2>Availability grid</h2><p>{visible.length} facilities · {grid.slots.length} slots · click a free cell to reserve it</p></div><div className="legend"><span><i className="legend-swatch free" /> Free</span><span><i className="legend-swatch booked" /> Booked</span><span><i className="legend-swatch held" /> Held</span><span><i className="legend-swatch closed" /> Closed</span></div></div><div className="grid-scroll"><table className="availability-grid"><thead><tr><th scope="col" className="facility-head">Facility</th>{grid.slots.map((slot) => <th key={slot.start} scope="col">{displayTime(slot.start)}</th>)}</tr></thead><tbody>{visible.map((facility) => { const index = grid.facilities.findIndex((item) => item.id === facility.id); return <tr key={facility.id}><th scope="row" className="facility-cell"><span className={`sport-icon sport-${facility.sport}`}>{facility.sport.slice(0, 1).toUpperCase()}</span><span><strong>{facility.name}</strong><small>{facility.is_exclusive ? "Exclusive court" : `${byId.get(facility.id)?.capacity || "Shared"} capacity`}</small></span></th>{grid.slots.map((slot, slotIndex) => { const state = grid.grid[index]?.[slotIndex] || "closed"; const source = byId.get(facility.id); return <td key={slot.start}><button type="button" disabled={!source || !["free", "filling"].includes(state)} className={`slot-cell state-${state}`} onClick={() => source && onPick({ facility: source, start: slot.start, end: slot.end, state })} aria-label={`${facility.name}, ${displayTime(slot.start)}, ${state}`}>{state === "free" ? <span>Open</span> : state === "filling" ? <span>Filling</span> : state === "booked" ? <span>Booked</span> : state === "held" ? <span>Held</span> : <span>Closed</span>}</button></td>; })}</tr>; })}</tbody></table></div></div>}
    <div className="discovery-footer"><div className="footer-callout"><ShieldCheck size={19} /><div><strong>Every booking has one source of truth.</strong><p>Everyone can browse quickly. Only a committed booking wins the slot.</p></div></div><div className="footer-callout"><Clock3 size={19} /><div><strong>Need a busy slot?</strong><p>Tap a full exclusive court to join its waitlist.</p></div></div></div>
  </section>;
}

function BookingModal({ choice, session, onClose, onToast, onRefresh, onWaitlist }: { choice: SlotChoice; session: Session; onClose: () => void; onToast: (message: string) => void; onRefresh: () => void; onWaitlist: (entry: WaitlistEntry) => void }) {
  const [duration, setDuration] = useState(choice.facility.slot_minutes || 60);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [booked, setBooked] = useState<Booking | null>(null);
  const min = choice.facility.min_duration_minutes || choice.facility.slot_minutes;
  const max = choice.facility.max_duration_minutes || min;
  const options = Array.from({ length: Math.floor((max - min) / (choice.facility.slot_minutes || 60)) + 1 }, (_, i) => min + i * (choice.facility.slot_minutes || 60));
  const reserve = async () => {
    setBusy(true); setError(null);
    try { const result = await api.createBooking({ facility_id: choice.facility.id, start: choice.start, duration_minutes: duration }, session.token); setBooked(result); onRefresh(); onToast(`Booking ${result.reference} confirmed`); } catch (err) { setError(err instanceof ApiError ? err : new ApiError(0, { error: "UNKNOWN", message: friendlyError(err) })); } finally { setBusy(false); }
  };
  const join = async () => {
    setBusy(true); setError(null);
    try { const result = await api.joinWaitlist({ facility_id: choice.facility.id, start: choice.start, duration_minutes: duration }, session.token); onWaitlist(result); onToast(`You are #${result.position} on the waitlist`); onClose(); } catch (err) { setError(err instanceof ApiError ? err : new ApiError(0, { error: "UNKNOWN", message: friendlyError(err) })); } finally { setBusy(false); }
  };
  return <div className="modal-backdrop" role="presentation"><div className="modal" role="dialog" aria-modal="true" aria-labelledby="booking-title"><div className="modal-header"><div><p className="eyebrow">Reserve a slot</p><h2 id="booking-title">{choice.facility.name}</h2></div><button className="icon-button" type="button" onClick={onClose} aria-label="Close booking dialog"><X size={19} /></button></div>{booked ? <div className="success-state"><div className="success-icon"><Check size={26} /></div><p className="eyebrow">You are all set</p><h3>{booked.reference}</h3><p>{booked.facility || choice.facility.name}<br />{displayDate(booked.start)} · {displayTime(booked.start)} to {displayTime(booked.end)}</p><div className="reminder"><Clock3 size={17} /><span>Check-in opens 10 minutes before your slot and has a 15-minute grace window after it starts.</span></div><button className="primary-button" type="button" onClick={onClose}>Done</button></div> : <><div className="booking-summary"><div><span>When</span><strong>{displayDate(choice.start)}</strong><strong>{displayTime(choice.start)} - {displayTime(new Date(new Date(choice.start).getTime() + duration * 60000).toISOString())}</strong></div><div><span>Format</span><strong>{choice.facility.is_exclusive ? "Exclusive court" : `Shared · ${choice.remaining ?? choice.facility.capacity} spots left`}</strong></div></div><label className="field-label" htmlFor="duration">Duration</label><select id="duration" value={duration} onChange={(e) => setDuration(Number(e.target.value))}>{options.map((minutes) => <option key={minutes} value={minutes}>{minutes / 60 >= 1 ? `${minutes / 60} hour${minutes > 60 ? "s" : ""}` : `${minutes} minutes`}</option>)}</select>{error && <div className="error-panel compact" role="alert"><X size={17} /><div><strong>{error.message}</strong>{error.body.alternatives?.length ? <p>Try {error.body.alternatives.map((item) => `${item.name} at ${item.start}`).join(", ")}.</p> : null}</div></div>}<div className="modal-actions"><button className="secondary-button" type="button" onClick={onClose}>Not now</button>{error?.body.waitlist_available && choice.facility.is_exclusive ? <button className="secondary-button" type="button" onClick={join} disabled={busy}>{busy ? "Joining" : "Join waitlist"}</button> : <button className="primary-button" type="button" onClick={reserve} disabled={busy}>{busy ? <><RefreshCw size={16} className="spin" /> Reserving</> : <>Reserve slot <ChevronRight size={16} /></>}</button>}</div></>}</div></div>;
}

function BookingsView({ session, facilities, waitlistEntries, onToast, onRefresh, onRemoveWaitlist }: { session: Session; facilities: Facility[]; waitlistEntries: WaitlistEntry[]; onToast: (message: string) => void; onRefresh: () => void; onRemoveWaitlist: (id: string) => void }) {
  const [data, setData] = useState<{ upcoming: Booking[]; past: Booking[] } | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [busyId, setBusyId] = useState("");
  const [checkinBooking, setCheckinBooking] = useState<Booking | null>(null);
  const load = () => { setLoading(true); api.bookings(session.token).then(setData).catch((err) => setError(friendlyError(err))).finally(() => setLoading(false)); };
  useEffect(load, [session.token]);
  const facilityName = (id: string) => facilities.find((facility) => facility.id === id)?.name || "Sports facility";
  const cancel = async (booking: Booking) => { setBusyId(booking.id); try { await api.cancelBooking(booking.id, session.token); onToast(`${booking.reference} cancelled`); load(); onRefresh(); } catch (err) { onToast(friendlyError(err)); } finally { setBusyId(""); } };
  const claim = async (booking: Booking) => { setBusyId(booking.id); try { await api.claimBooking(booking.id, session.token); onToast(`${booking.reference} claimed`); load(); onRefresh(); } catch (err) { onToast(friendlyError(err)); } finally { setBusyId(""); } };
  const leaveWaitlist = async (entry: WaitlistEntry) => { setBusyId(entry.id); try { await api.leaveWaitlist(entry.id, session.token); onRemoveWaitlist(entry.id); onToast("You left the waitlist"); } catch (err) { onToast(friendlyError(err)); } finally { setBusyId(""); } };
  const all = [...(data?.upcoming || []), ...(data?.past || [])];
  return <section className="view-stack"><div className="page-heading"><div><p className="eyebrow">Your schedule</p><h1>My bookings.</h1><p className="lede">Keep your references close. Check in from here when you arrive.</p></div><button className="secondary-button" type="button" onClick={load} disabled={loading}><RefreshCw size={16} className={loading ? "spin" : ""} /> Refresh</button></div>{error && <div className="error-panel" role="alert"><Activity size={18} /><p>{error}</p></div>}{loading ? <div className="panel"><Loading label="Loading your bookings" /></div> : !all.length && !waitlistEntries.length ? <Empty icon={Ticket} title="Your schedule is clear" body="Browse the campus grid to reserve your first slot." /> : <div className="booking-columns"><div className="booking-list-panel"><div className="panel-header"><div><h2>Upcoming</h2><p>Active reservations and promotion offers</p></div><span className="count-pill">{data?.upcoming.length || 0}</span></div>{!data?.upcoming.length ? <p className="muted-copy">No upcoming reservations.</p> : <div className="booking-list">{data.upcoming.map((booking) => <BookingCard key={booking.id} booking={booking} facilityName={facilityName(booking.facility_id)} busy={busyId === booking.id} onCancel={() => cancel(booking)} onClaim={() => claim(booking)} onCheckin={() => setCheckinBooking(booking)} />)}</div>}</div><div className="booking-list-panel"><div className="panel-header"><div><h2>Past and queue</h2><p>History plus your local waitlist offers</p></div><span className="count-pill">{(data?.past.length || 0) + waitlistEntries.length}</span></div>{waitlistEntries.length > 0 && <div className="queue-list">{waitlistEntries.map((entry) => <div className="queue-row" key={entry.id}><span className="queue-number">#{entry.position}</span><div><strong>{facilityName(entry.facility_id)}</strong><small>{displayDate(entry.start)} · {displayTime(entry.start)}</small></div><StatusBadge state={entry.status} /><button className="text-button danger" type="button" onClick={() => leaveWaitlist(entry)} disabled={busyId === entry.id}>Leave</button></div>)}</div>}{!data?.past.length ? <p className="muted-copy">No past bookings yet.</p> : <div className="booking-list">{data.past.map((booking) => <BookingCard key={booking.id} booking={booking} facilityName={facilityName(booking.facility_id)} past />)}</div>}</div></div>}{checkinBooking && <CheckinModal booking={checkinBooking} session={session} onClose={() => setCheckinBooking(null)} onToast={onToast} onComplete={() => { setCheckinBooking(null); load(); }} />}</section>;
}

function BookingCard({ booking, facilityName, busy, past, onCancel, onClaim, onCheckin }: { booking: Booking; facilityName: string; busy?: boolean; past?: boolean; onCancel?: () => void; onClaim?: () => void; onCheckin?: () => void }) {
  return <article className="booking-card"><div className="booking-card-top"><div><p className="eyebrow">{booking.reference}</p><h3>{booking.facility || facilityName}</h3></div><StatusBadge state={booking.status} /></div><div className="booking-time"><CalendarDays size={16} /><strong>{displayDate(booking.start)}</strong><span>{displayTime(booking.start)} - {displayTime(booking.end)}</span></div>{!past && <div className="booking-card-actions">{booking.status === "HELD" && onClaim && <button className="primary-button small" type="button" onClick={onClaim} disabled={busy}>{busy ? "Claiming" : "Claim offer"}</button>}{booking.status === "CONFIRMED" && onCheckin && <button className="secondary-button small" type="button" onClick={onCheckin}><CheckCircle2 size={15} /> Check in</button>}{["CONFIRMED", "HELD"].includes(booking.status) && onCancel && <button className="text-button danger" type="button" onClick={onCancel} disabled={busy}>Cancel</button>}</div>}</article>;
}

function CheckinModal({ booking, session, onClose, onToast, onComplete }: { booking: Booking; session: Session; onClose: () => void; onToast: (message: string) => void; onComplete: () => void }) {
  const [token, setToken] = useState(""); const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  const submit = async (event: React.FormEvent) => { event.preventDefault(); setBusy(true); setError(""); try { await api.checkIn(booking.id, token.trim(), session.token); onToast(`${booking.reference} checked in. Enjoy the game.`); onComplete(); } catch (err) { setError(friendlyError(err)); } finally { setBusy(false); } };
  return <div className="modal-backdrop"><div className="modal narrow" role="dialog" aria-modal="true" aria-labelledby="checkin-title"><div className="modal-header"><div><p className="eyebrow">Venue check-in</p><h2 id="checkin-title">{booking.reference}</h2></div><button className="icon-button" type="button" onClick={onClose} aria-label="Close check-in dialog"><X size={19} /></button></div><p className="modal-copy">Ask the venue display for its current code, then enter it here. Codes rotate regularly.</p><form className="stack-form" onSubmit={submit}><label htmlFor="checkin-token">Venue code</label><input id="checkin-token" value={token} onChange={(e) => setToken(e.target.value)} placeholder="Paste the venue token" autoComplete="one-time-code" required />{error && <div className="inline-error" role="alert"><X size={16} />{error}</div>}<div className="modal-actions"><button className="secondary-button" type="button" onClick={onClose}>Cancel</button><button className="primary-button" type="submit" disabled={busy || !token.trim()}>{busy ? "Checking in" : <><CheckCircle2 size={16} /> Check in</>}</button></div></form></div></div>;
}

function CheckinView({ session, onToast }: { session: Session; onToast: (message: string) => void }) {
  const [data, setData] = useState<{ upcoming: Booking[]; past: Booking[] } | null>(null); const [loading, setLoading] = useState(true); const [selected, setSelected] = useState<Booking | null>(null); const [error, setError] = useState("");
  useEffect(() => { api.bookings(session.token).then(setData).catch((err) => setError(friendlyError(err))).finally(() => setLoading(false)); }, [session.token]);
  const eligible = data?.upcoming.filter((booking) => booking.status === "CONFIRMED") || [];
  return <section className="view-stack"><div className="page-heading"><div><p className="eyebrow">Arrive, scan, play</p><h1>Check-in.</h1><p className="lede">Use the rotating code shown at the facility. It proves you are there without exposing your booking to anyone else.</p></div><div className="checkin-mark"><ShieldCheck size={29} /><span>Venue-verified</span></div></div><div className="checkin-guide"><div className="guide-step"><span>01</span><strong>Open your booking</strong><p>Choose the reservation you are attending today.</p></div><div className="guide-line" /><div className="guide-step"><span>02</span><strong>Read the venue display</strong><p>Codes rotate and only work for the right facility.</p></div><div className="guide-line" /><div className="guide-step"><span>03</span><strong>Enter and go</strong><p>Check-in opens 10 minutes early and closes after the grace period.</p></div></div>{error && <div className="error-panel" role="alert"><Activity size={18} /><p>{error}</p></div>}{loading ? <div className="panel"><Loading label="Finding eligible bookings" /></div> : !eligible.length ? <Empty icon={CheckCircle2} title="Nothing to check in yet" body="Your confirmed bookings will appear here on the day." /> : <div className="checkin-list">{eligible.map((booking) => <article className="checkin-card" key={booking.id}><div><p className="eyebrow">{booking.reference}</p><h2>{booking.facility || "Sports facility"}</h2><p><CalendarDays size={15} /> {displayDate(booking.start)} · {displayTime(booking.start)} - {displayTime(booking.end)}</p></div><button className="primary-button" type="button" onClick={() => setSelected(booking)}><CheckCircle2 size={16} /> Enter venue code</button></article>)}</div>}{selected && <CheckinModal booking={selected} session={session} onClose={() => setSelected(null)} onToast={onToast} onComplete={() => { setSelected(null); }} />}</section>;
}

function ManagerView({ session, facilities, onToast }: { session: Session; facilities: Facility[]; onToast: (message: string) => void }) {
  const [tab, setTab] = useState<"operations" | "analytics" | "venue">(session.role === "MANAGER" ? "operations" : "analytics");
  return <section className="view-stack"><div className="page-heading"><div><p className="eyebrow">Sports board workspace</p><h1>Manager console.</h1><p className="lede">Keep the venue usable, understand demand, and make every operational change visible.</p></div><div className="role-lock"><ShieldCheck size={16} /> {session.role.toLowerCase()} access</div></div><div className="console-tabs" role="tablist">{session.role === "MANAGER" && <button type="button" className={tab === "operations" ? "selected" : ""} onClick={() => setTab("operations")}><Activity size={16} /> Operations</button>}<button type="button" className={tab === "analytics" ? "selected" : ""} onClick={() => setTab("analytics")}><BarChart3 size={16} /> Analytics</button>{session.role === "MANAGER" && <button type="button" className={tab === "venue" ? "selected" : ""} onClick={() => setTab("venue")}><ShieldCheck size={16} /> Venue token</button>}</div>{tab === "operations" && session.role === "MANAGER" && <ClosureBoard session={session} facilities={facilities} onToast={onToast} />}{tab === "analytics" && <AnalyticsBoard session={session} />}{tab === "venue" && session.role === "MANAGER" && <VenueTokenBoard session={session} facilities={facilities} onToast={onToast} />}</section>;
}

function ClosureBoard({ session, facilities, onToast }: { session: Session; facilities: Facility[]; onToast: (message: string) => void }) {
  const [closures, setClosures] = useState<Closure[]>([]); const [loading, setLoading] = useState(true); const [busy, setBusy] = useState(false); const [error, setError] = useState(""); const [form, setForm] = useState({ facility_id: "", start: `${todayISO()}T18:00`, end: `${todayISO()}T19:00`, reason: "" });
  const load = () => { setLoading(true); api.closures(session.token).then((result) => setClosures(result.closures || [])).catch((err) => setError(friendlyError(err))).finally(() => setLoading(false)); };
  useEffect(() => { if (!form.facility_id && facilities[0]) setForm((current) => ({ ...current, facility_id: facilities[0].id })); }, [facilities, form.facility_id]);
  useEffect(load, [session.token]);
  const create = async (event: React.FormEvent) => { event.preventDefault(); setBusy(true); setError(""); try { const result = await api.createClosure({ facility_id: form.facility_id, start: toRFC3339(form.start), end: toRFC3339(form.end), reason: form.reason }, session.token); onToast(`${result.facility || "Facility"} is closed for that window`); setForm((current) => ({ ...current, reason: "" })); load(); } catch (err) { setError(friendlyError(err)); } finally { setBusy(false); } };
  const reopen = async (closure: Closure) => { setBusy(true); try { await api.reopenClosure(closure.id, session.token); onToast("Closure reopened"); load(); } catch (err) { onToast(friendlyError(err)); } finally { setBusy(false); } };
  return <div className="manager-grid"><div className="panel closure-form"><div className="panel-header"><div><h2>Close a facility</h2><p>Block a window and see every booking affected before acting.</p></div></div><form className="stack-form" onSubmit={create}><label htmlFor="closure-facility">Facility</label><select id="closure-facility" value={form.facility_id} onChange={(e) => setForm({ ...form, facility_id: e.target.value })}>{facilities.map((facility) => <option key={facility.id} value={facility.id}>{facility.name}</option>)}</select><div className="two-fields"><div><label htmlFor="closure-start">Start</label><input id="closure-start" type="datetime-local" value={form.start} onChange={(e) => setForm({ ...form, start: e.target.value })} required /></div><div><label htmlFor="closure-end">End</label><input id="closure-end" type="datetime-local" value={form.end} onChange={(e) => setForm({ ...form, end: e.target.value })} required /></div></div><label htmlFor="closure-reason">Reason <span className="optional">optional</span></label><input id="closure-reason" value={form.reason} onChange={(e) => setForm({ ...form, reason: e.target.value })} placeholder="Maintenance, tournament, weather..." />{error && <div className="inline-error" role="alert"><X size={16} />{error}</div>}<button className="primary-button" type="submit" disabled={busy || !form.facility_id}>{busy ? "Publishing closure" : <>Publish closure <ChevronRight size={16} /></>}</button></form></div><div className="panel closures-panel"><div className="panel-header"><div><h2>Closure board</h2><p>Upcoming and active blocked windows</p></div><button className="icon-button" type="button" onClick={load} aria-label="Refresh closures"><RefreshCw size={16} className={loading ? "spin" : ""} /></button></div>{loading ? <Loading label="Loading closure board" /> : !closures.length ? <Empty icon={Check} title="No closures" body="All facilities are open unless a booking occupies the slot." /> : <div className="closure-list">{closures.map((closure) => <article className="closure-card" key={closure.id}><div className="closure-card-top"><div><h3>{closure.facility || facilities.find((f) => f.id === closure.facility_id)?.name || "Facility"}</h3><p>{displayDate(closure.start)} · {displayTime(closure.start)} - {displayTime(closure.end)}</p></div><StatusBadge state={closure.status} /></div>{closure.reason && <p className="reason">{closure.reason}</p>}<div className="affected"><Users size={15} /><span>{closure.affected_bookings?.length || 0} affected booking{closure.affected_bookings?.length === 1 ? "" : "s"}</span>{closure.affected_bookings?.slice(0, 2).map((booking) => <small key={booking.booking_id}>{booking.roll_no} · {booking.name}</small>)}</div>{closure.status === "BLOCKED" && <button className="text-button" type="button" onClick={() => reopen(closure)} disabled={busy}>Reopen window</button>}</article>)}</div>}</div></div>;
}

function AnalyticsBoard({ session }: { session: Session }) {
  const [from, setFrom] = useState(new Date(Date.now() - 29 * 86400000).toLocaleDateString("en-CA")); const [to, setTo] = useState(todayISO()); const [report, setReport] = useState<AnalyticsReport | null>(null); const [loading, setLoading] = useState(false); const [error, setError] = useState("");
  const load = useCallback(() => { setLoading(true); setError(""); api.analytics(from, to, session.token).then(setReport).catch((err) => setError(friendlyError(err))).finally(() => setLoading(false)); }, [from, session.token, to]);
  useEffect(() => { load(); }, [load]);
  const maxHeat = Math.max(1, ...(report?.peak_demand.cells.flat() || [1])); const topUtil = report?.utilisation.reduce<{ name: string; value: number }[]>((all, row) => { const current = all.find((item) => item.name === row.facility_name); if (current) current.value += row.utilisation; else all.push({ name: row.facility_name, value: row.utilisation }); return all; }, []).sort((a, b) => b.value - a.value).slice(0, 5) || [];
  return <div className="analytics-stack"><div className="analytics-filter"><label>From<input type="date" value={from} onChange={(e) => setFrom(e.target.value)} /></label><ArrowLeft size={15} className="date-arrow" /><label>To<input type="date" value={to} onChange={(e) => setTo(e.target.value)} /></label><button className="primary-button" type="button" onClick={load} disabled={loading}>{loading ? "Loading" : "Apply range"}</button></div>{error && <div className="error-panel" role="alert"><Activity size={18} /><p>{error}</p></div>}{!report && loading ? <div className="panel"><Loading label="Building manager report" /></div> : report && <><div className="metric-row"><Metric icon={BarChart3} label="Bookings analysed" value={String(report.no_show.reduce((sum, item) => sum + item.total, 0))} note={`${report.from} to ${report.to}`} /><Metric icon={Users} label="Promoted from queue" value={String(report.slot_recovery.promoted)} note={`${Math.round(report.slot_recovery.rate * 100)}% recovery rate`} /><Metric icon={ShieldCheck} label="No-show rate" value={`${Math.round((report.no_show.reduce((sum, item) => sum + item.no_shows, 0) / Math.max(1, report.no_show.reduce((sum, item) => sum + item.total, 0))) * 100)}%`} note="Across attended slots" /><Metric icon={Flame} label="Peak demand" value={String(report.peak_demand.peak.count)} note={report.peak_demand.peak.count ? `${report.peak_demand.weekdays[report.peak_demand.peak.weekday]} at ${report.peak_demand.peak.hour}:00` : "No demand yet"} /></div><div className="analytics-columns"><div className="panel"><div className="panel-header"><div><h2>Utilisation by facility</h2><p>Booked hours compared with available hours</p></div></div>{!topUtil.length ? <p className="muted-copy">No utilisation data in this range.</p> : <div className="util-list">{topUtil.map((item) => <div className="util-row" key={item.name}><div><strong>{item.name}</strong><span>{Math.round(item.value / Math.max(1, report.utilisation.filter((row) => row.facility_name === item.name).length) * 100)}%</span></div><div className="bar"><i style={{ width: `${Math.min(100, item.value / Math.max(1, report.utilisation.filter((row) => row.facility_name === item.name).length) * 100)}%` }} /></div></div>)}</div>}</div><div className="panel"><div className="panel-header"><div><h2>Demand heatmap</h2><p>Requests by weekday and hour</p></div></div><div className="heatmap-wrap"><div className="heatmap-hours">{report.peak_demand.hours.filter((_, i) => i % 4 === 0).map((hour) => <span key={hour}>{hour}</span>)}</div><div className="heatmap"><div className="heatmap-y">{report.peak_demand.weekdays.map((day) => <span key={day}>{day}</span>)}</div><div className="heatmap-cells">{report.peak_demand.cells.map((row, day) => row.map((count, hour) => <span key={`${day}-${hour}`} title={`${report.peak_demand.weekdays[day]} ${hour}:00, ${count} requests`} style={{ opacity: count ? 0.25 + count / maxHeat * 0.75 : 0.08 }} />))}</div></div></div></div></div><div className="panel no-show-panel"><div className="panel-header"><div><h2>Attendance by facility</h2><p>Use this to target reminders and venue support</p></div></div><div className="table-wrap"><table className="data-table"><thead><tr><th>Facility</th><th>Total</th><th>No-shows</th><th>Rate</th></tr></thead><tbody>{report.no_show.map((row) => <tr key={row.facility_id}><td>{row.facility_name}</td><td>{row.total}</td><td>{row.no_shows}</td><td><strong>{Math.round(row.rate * 100)}%</strong></td></tr>)}</tbody></table></div></div></>}</div>;
}

function Metric({ icon: Icon, label, value, note }: { icon: typeof BarChart3; label: string; value: string; note: string }) { return <div className="metric"><span className="metric-icon"><Icon size={17} /></span><span className="metric-label">{label}</span><strong>{value}</strong><small>{note}</small></div>; }

function VenueTokenBoard({ session, facilities, onToast }: { session: Session; facilities: Facility[]; onToast: (message: string) => void }) {
  const [facility, setFacility] = useState(facilities[0]?.id || ""); const [token, setToken] = useState<{ token: string; refresh_in_seconds: number; valid_for_seconds: number; issued_at: string } | null>(null); const [loading, setLoading] = useState(false); const [remaining, setRemaining] = useState(0);
  useEffect(() => { if (!facility && facilities[0]) setFacility(facilities[0].id); }, [facilities, facility]);
  const mint = useCallback(async () => { setLoading(true); try { const result = await api.checkinToken(facility, session.token); setToken(result); setRemaining(result.refresh_in_seconds); } catch (err) { onToast(friendlyError(err)); } finally { setLoading(false); } }, [facility, onToast, session.token]);
  useEffect(() => { if (!token) return; const timer = window.setInterval(() => setRemaining((current) => { if (current <= 1) { void mint(); return token.refresh_in_seconds; } return current - 1; }), 1000); return () => window.clearInterval(timer); }, [mint, token]);
  return <div className="token-layout"><div className="panel token-panel"><div className="panel-header"><div><h2>Venue display token</h2><p>Keep this screen at the facility entrance. Tokens rotate automatically.</p></div><ShieldCheck size={22} className="green-icon" /></div><label className="field-label" htmlFor="token-facility">Facility</label><select id="token-facility" value={facility} onChange={(e) => { setFacility(e.target.value); setToken(null); }}>{facilities.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select>{!token ? <div className="token-empty"><ShieldCheck size={32} /><p>Generate a venue code to begin.</p><button className="primary-button" type="button" onClick={mint} disabled={loading || !facility}>{loading ? "Generating" : "Generate current code"}</button></div> : <div className="token-display"><div className="token-code">{token.token}</div><div className="token-countdown"><span>Refreshes in</span><strong>{remaining}s</strong></div><small>Accepted for {token.valid_for_seconds}s · issued {displayTime(token.issued_at)}</small></div>}</div><div className="panel token-notes"><h2>Check-in protocol</h2><div className="protocol-item"><span>1</span><p><strong>Display only at the venue.</strong><br />The code is proof of physical presence, not a student-facing booking secret.</p></div><div className="protocol-item"><span>2</span><p><strong>Rotate without refreshing.</strong><br />The display updates before the current code expires, so slow scans still work.</p></div><div className="protocol-item"><span>3</span><p><strong>Review no-shows in Analytics.</strong><br />A check-in is recorded once, even if a student scans twice.</p></div></div></div>;
}

function RaceView({ session, facilities }: { session: Session; facilities: Facility[] }) {
  const [facility, setFacility] = useState(facilities[0]?.id || ""); const [start, setStart] = useState(`${todayISO()}T18:00`); const [n, setN] = useState(500); const [duration, setDuration] = useState(60); const [result, setResult] = useState<RaceResult | null>(null); const [resetState, setResetState] = useState<ResetResult | null>(null); const [loading, setLoading] = useState(false); const [error, setError] = useState("");
  useEffect(() => { if (!facility && facilities[0]) setFacility(facilities[0].id); }, [facilities, facility]);
  const run = async () => { setLoading(true); setError(""); setResetState(null); try { setResult(await api.race({ facility_id: facility, start: toRFC3339(start), duration_minutes: duration, n }, session.token)); } catch (err) { setError(friendlyError(err)); } finally { setLoading(false); } };
  const reset = async () => { setLoading(true); setError(""); try { setResetState(await api.resetRace({ facility_id: facility, start: toRFC3339(start), duration_minutes: duration }, session.token)); setResult(null); } catch (err) { setError(friendlyError(err)); } finally { setLoading(false); } };
  return <section className="race-stack"><div className="page-heading"><div><p className="eyebrow">Dev mode proof</p><h1>Race console.</h1><p className="lede">Fire concurrent attempts at one slot. The only number that matters is the database read-back.</p></div><div className="proof-lock"><Trophy size={18} /> Constraint proof</div></div><div className="race-controls panel"><div className="race-control-heading"><div><h2>Set the contention</h2><p>All attempts hit the same facility and start time in-process.</p></div><span className="dev-badge">DEV ONLY</span></div><div className="race-fields"><label>Facility<select value={facility} onChange={(e) => setFacility(e.target.value)}>{facilities.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label><label>Slot start<input type="datetime-local" value={start} onChange={(e) => setStart(e.target.value)} /></label><label>Attempts<input type="number" min={1} max={5000} value={n} onChange={(e) => setN(Number(e.target.value))} /></label><label>Duration<select value={duration} onChange={(e) => setDuration(Number(e.target.value))}><option value={60}>60 minutes</option><option value={120}>120 minutes</option></select></label></div><div className="race-actions"><button className="primary-button" type="button" onClick={run} disabled={loading || !facility}>{loading ? <><RefreshCw size={16} className="spin" /> Running race</> : <>Run {n}-request race <ChevronRight size={16} /></>}</button><button className="secondary-button" type="button" onClick={reset} disabled={loading || !facility}><RefreshCw size={16} /> Reset slot</button></div>{error && <div className="inline-error" role="alert"><X size={16} />{error} In OIDC mode, the dev-only race endpoint is intentionally unavailable.</div>}</div>{resetState ? <div className="proof-banner"><div className="proof-number"><span>Fresh database count</span><strong>{resetState.db_count}</strong></div><div><strong>Slot reset and ready.</strong><p>{resetState.cancelled} booking{resetState.cancelled === 1 ? "" : "s"} cleared. Run the race again when ready.</p></div><CheckCircle2 size={27} /></div> : result ? <div className="race-result"><div className="proof-banner"><div className="proof-number"><span>Fresh database count</span><strong>{result.db_count}</strong></div><div><strong>{result.db_count === 1 ? "One winner. No double booking." : "The invariant needs attention."}</strong><p>This count was read after every concurrent attempt completed.</p></div><CheckCircle2 size={27} /></div><div className="race-metrics"><div><span>Confirmed</span><strong className="green-text">{result.confirmed}</strong></div><div><span>409 conflicts</span><strong>{result.conflict_409}</strong></div><div><span>Other responses</span><strong>{result.other}</strong></div><div><span>Elapsed</span><strong>{result.elapsed_ms}<small>ms</small></strong></div><div><span>Reject p99</span><strong>{result.reject_p99_ms}<small>ms</small></strong></div><div><span>Start spread</span><strong>{result.start_spread_ms}<small>ms</small></strong></div></div><div className="race-lower"><div className="panel winner-panel"><div className="panel-header"><div><h2>Winner</h2><p>The booking that actually committed</p></div><Trophy size={19} /></div>{result.winner ? <div className="winner-detail"><span className="winner-crown"><Trophy size={21} /></span><div><strong>{result.winner.reference}</strong><span>{result.winner.user}</span><small>{result.winner.booking_id}</small></div></div> : <p className="muted-copy">No winner in this run. The slot was already occupied.</p>}</div><div className="panel proof-explainer"><h2>What this proves</h2><p>The handler does not read availability before writing. PostgreSQL decides the winner using the exclusion constraint, and every loser receives a clean conflict.</p><div className="constraint-code">EXCLUDE USING gist<br />(facility_id WITH =, during WITH &&)</div></div></div>{result.errors?.length ? <div className="error-panel"><Activity size={18} /><div><strong>Unclassified responses</strong>{result.errors.map((item) => <p key={item}>{item}</p>)}</div></div> : null}</div> : <div className="race-empty"><Trophy size={36} /><h2>Ready when you are</h2><p>Reset the slot, run the contention, and watch one database row survive.</p></div>}</section>;
}
