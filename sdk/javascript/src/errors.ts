// Typed error hierarchy. Every failure the SDK surfaces is a LoopSandboxError
// subclass, so callers can `catch (e) { if (e instanceof NotFoundError) ... }`.
//
// SECURITY: an error NEVER carries the API key or any request credential. Messages
// are built only from the HTTP status and the server-provided error body (which the
// server itself redacts). See errorFromResponse — it receives no auth material.

/** Base class for every error the SDK throws. */
export class LoopSandboxError extends Error {
  /** HTTP status code (0 for client-side/network failures). */
  readonly status: number;
  /** Machine-readable server error code (e.g. "not_found", "conflict"). */
  readonly code: string;
  /** X-Request-ID for correlation with server logs, when available. */
  readonly requestId: string | undefined;

  constructor(message: string, status: number, code: string, requestId?: string) {
    super(message);
    // new.target keeps the subclass name even after transpilation.
    this.name = new.target.name;
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

/** 400 — the request was malformed or violated a validation rule. */
export class ValidationError extends LoopSandboxError {}
/** 401 — missing or invalid credentials. */
export class UnauthorizedError extends LoopSandboxError {}
/** 403 — authenticated but not permitted. */
export class ForbiddenError extends LoopSandboxError {}
/** 404 — resource not found (or not visible to this tenant — US-05). */
export class NotFoundError extends LoopSandboxError {}
/** 409 — idempotency conflict or invalid state transition. */
export class ConflictError extends LoopSandboxError {}
/** 413 — payload exceeded the server size limit. */
export class PayloadTooLargeError extends LoopSandboxError {}
/** 402: a plan quota was reached (code "quota_exceeded"). */
export class QuotaExceededError extends LoopSandboxError {}
/** 429: the API key went over its request rate limit (code "rate_limited"). */
export class RateLimitError extends LoopSandboxError {
  /** Seconds to wait before the next request, from the Retry-After header.
   *  Undefined when the server sent no usable header. */
  readonly retryAfterSeconds: number | undefined;

  constructor(message: string, status: number, code: string, requestId?: string, retryAfterSeconds?: number) {
    super(message, status, code, requestId);
    this.retryAfterSeconds = retryAfterSeconds;
  }
}
/** 408/504 or a client-side AbortSignal timeout. */
export class TimeoutError extends LoopSandboxError {}
/** 5xx — the server failed to process the request. */
export class ServerError extends LoopSandboxError {}
/** 501: the active deployment does not support this capability (code
 *  "not_implemented"). It is a ServerError, so existing handlers keep working;
 *  client.capabilities.get() lists what the deployment supports. */
export class NotImplementedError extends ServerError {}

/** Maps an HTTP status + server error body to the right typed error. */
export function errorFromResponse(
  status: number,
  code: string,
  message: string,
  requestId?: string,
  retryAfterSeconds?: number,
): LoopSandboxError {
  switch (status) {
    case 402:
      return new QuotaExceededError(message, status, code, requestId);
    case 501:
      return new NotImplementedError(message, status, code, requestId);
    case 400:
      return new ValidationError(message, status, code, requestId);
    case 401:
      return new UnauthorizedError(message, status, code, requestId);
    case 403:
      return new ForbiddenError(message, status, code, requestId);
    case 404:
      return new NotFoundError(message, status, code, requestId);
    case 409:
      return new ConflictError(message, status, code, requestId);
    case 413:
      return new PayloadTooLargeError(message, status, code, requestId);
    case 429:
      return new RateLimitError(message, status, code, requestId, retryAfterSeconds);
    case 408:
    case 504:
      return new TimeoutError(message, status, code, requestId);
    default:
      if (status >= 500) return new ServerError(message, status, code, requestId);
      return new LoopSandboxError(message, status, code, requestId);
  }
}
