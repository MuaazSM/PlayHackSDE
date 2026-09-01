import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, makeIdempotencyKey } from "./api";

afterEach(() => vi.restoreAllMocks());

describe("PlayHack API client", () => {
  it("sends the dev login payload and maps the session response", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({
      token: "signed-token",
      user_id: "u-1",
      roll_no: "student01",
      role: "STUDENT",
    }), { status: 200, headers: { "Content-Type": "application/json" } }));

    const result = await api.login("student01");
    expect(result.role).toBe("STUDENT");
    expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining("/api/v1/dev/login"), expect.objectContaining({ method: "POST" }));
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ roll_no: "student01" });
  });

  it("preserves the machine-readable error envelope", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({
      error: "SLOT_TAKEN",
      message: "Tennis Court 1 was booked moments ago.",
      waitlist_available: true,
      alternatives: [{ facility_id: "f-2", name: "Tennis Court 2", start: "19:00" }],
    }), { status: 409, headers: { "Content-Type": "application/json" } }));

    const error = await api.createBooking({ facility_id: "f-1", start: "2026-08-31T12:30:00.000Z", duration_minutes: 60 }, "token").catch((value) => value);
    expect(error).toBeInstanceOf(ApiError);
    expect((error as ApiError).code).toBe("SLOT_TAKEN");
    expect((error as ApiError).body.waitlist_available).toBe(true);
    expect((error as ApiError).body.alternatives?.[0].name).toBe("Tennis Court 2");
  });

  it("creates non-empty idempotency keys", () => {
    const key = makeIdempotencyKey();
    expect(key).toEqual(expect.any(String));
    expect(key.length).toBeGreaterThan(8);
  });

  it("models the reset response separately from a race result", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({
      facility_id: "f-1",
      start: "2026-08-31T12:30:00.000Z",
      end: "2026-08-31T13:30:00.000Z",
      cancelled: 1,
      db_count: 0,
    }), { status: 200, headers: { "Content-Type": "application/json" } }));

    const result = await api.resetRace({ facility_id: "f-1", start: "2026-08-31T12:30:00.000Z", duration_minutes: 60 }, "token");
    expect(result.cancelled).toBe(1);
    expect(result.db_count).toBe(0);
  });
});
