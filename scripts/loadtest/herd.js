// Go-live herd load test (docs/12-implementation-plan.md, Milestone 9:
// "Load test the go-live herd: 1,000 simulated starts in 60 s, 2,000
// sockets, autosave p95 < 300 ms verified").
//
// Two scenarios run concurrently against fixtures from
// `macquiz loadtest-seed` (scripts/loadtest/run.sh drives both):
//
//   steady - STEADY_STUDENTS (default 1000) students already logged in and
//            mid-attempt when the test starts, each holding its attempt
//            socket open and autosaving for the whole run. Models docs/01's
//            "2,000 concurrent students in a live window" baseline.
//   herd   - HERD_STUDENTS (default 1000) fresh logins + attempt starts
//            arriving at a constant rate over HERD_WINDOW_S seconds (default
//            60s) - the go-live spike itself.
//
// Together, around HERD_WINDOW_S into the run, both pools are live at once:
// ~2,000 concurrent attempt:{id} sockets, each autosaving every
// AUTOSAVE_INTERVAL_S seconds and heartbeating every 10s (matching the real
// player's cadence, docs/05 section 5). The autosave_ms threshold is the
// requirement's actual pass/fail line; attempt_start_ok and ws_connect_ok
// catch a herd that can't even log its students in or open their sockets.
//
// Run via scripts/loadtest/run.sh, which seeds fixtures first and passes
// QUIZ_ID/BASE_URL/WS_BASE_URL automatically. To run by hand:
//
//   k6 run -e BASE_URL=http://localhost:8080 -e QUIZ_ID=<id> scripts/loadtest/herd.js
//
import http from "k6/http";
import ws from "k6/ws";
import { check } from "k6";
import { Trend, Rate } from "k6/metrics";
import exec from "k6/execution";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const WS_BASE_URL = __ENV.WS_BASE_URL || BASE_URL.replace(/^http/, "ws");
const QUIZ_ID = __ENV.QUIZ_ID;
if (!QUIZ_ID) {
  throw new Error("QUIZ_ID is required - run scripts/loadtest/run.sh, or pass -e QUIZ_ID=<id> after `macquiz loadtest-seed`");
}
const STUDENT_PASSWORD = __ENV.STUDENT_PASSWORD || "LoadTest!Pass123";
const STEADY_STUDENTS = parseInt(__ENV.STEADY_STUDENTS || "1000", 10);
const HERD_STUDENTS = parseInt(__ENV.HERD_STUDENTS || "1000", 10);
const HERD_WINDOW_S = parseInt(__ENV.HERD_WINDOW_S || "60", 10);
const STEADY_SESSION_S = parseInt(__ENV.STEADY_SESSION_S || "150", 10);
const HERD_SESSION_S = parseInt(__ENV.HERD_SESSION_S || "90", 10);
const AUTOSAVE_INTERVAL_S = parseInt(__ENV.AUTOSAVE_INTERVAL_S || "8", 10);
const HEARTBEAT_INTERVAL_S = 10;

const attemptStartMs = new Trend("attempt_start_ms", true);
const autosaveMs = new Trend("autosave_ms", true);
const attemptStartOk = new Rate("attempt_start_ok");
const wsConnectOk = new Rate("ws_connect_ok");

export const options = {
  scenarios: {
    steady: {
      executor: "per-vu-iterations",
      exec: "steadyStudent",
      vus: STEADY_STUDENTS,
      iterations: 1,
      maxDuration: `${STEADY_SESSION_S + 60}s`,
      gracefulStop: "60s",
    },
    herd: {
      executor: "constant-arrival-rate",
      exec: "herdStudent",
      rate: HERD_STUDENTS,
      timeUnit: `${HERD_WINDOW_S}s`,
      duration: `${HERD_WINDOW_S}s`,
      preAllocatedVUs: Math.min(HERD_STUDENTS, 1200),
      maxVUs: Math.min(HERD_STUDENTS + 300, 1500),
      gracefulStop: `${HERD_SESSION_S + 30}s`,
    },
  },
  thresholds: {
    // The docs/01 NFR this whole script exists to verify.
    autosave_ms: ["p(95)<300"],
    attempt_start_ok: ["rate>0.99"],
    ws_connect_ok: ["rate>0.99"],
    http_req_failed: ["rate<0.01"],
  },
};

function studentEmail(index) {
  return `loadtest-student-${String(index).padStart(5, "0")}@macquiz.load`;
}

// cookieHeader builds a "name=value; name2=value2" Cookie header from a k6
// http.Response's parsed Set-Cookie jar - ws.connect does not share the VU's
// implicit http cookie jar, so the access-token/refresh cookies login sets
// have to be forwarded onto both later http calls and the ws handshake
// explicitly.
function cookieHeader(res) {
  const parts = [];
  for (const name in res.cookies) {
    const jarred = res.cookies[name];
    if (jarred && jarred.length > 0) {
      parts.push(`${name}=${jarred[0].value}`);
    }
  }
  return parts.join("; ");
}

function runStudentSession(studentIndex, sessionDurationS) {
  const email = studentEmail(studentIndex);
  const loginRes = http.post(
    `${BASE_URL}/api/v1/auth/login`,
    JSON.stringify({ email, password: STUDENT_PASSWORD }),
    { headers: { "Content-Type": "application/json" }, tags: { name: "login" } },
  );
  if (!check(loginRes, { "login succeeded": (r) => r.status === 200 })) {
    return;
  }
  const cookie = cookieHeader(loginRes);

  const startRes = http.post(`${BASE_URL}/api/v1/quizzes/${QUIZ_ID}/attempts`, null, {
    headers: { Cookie: cookie },
    tags: { name: "start_attempt" },
  });
  attemptStartMs.add(startRes.timings.duration);
  const started = startRes.status === 200 || startRes.status === 201;
  attemptStartOk.add(started);
  if (!check(startRes, { "attempt started": () => started })) {
    return;
  }

  const attempt = JSON.parse(startRes.body);
  const attemptID = attempt.attempt.id;
  const questions = attempt.questions || [];

  const res = ws.connect(`${WS_BASE_URL}/ws/attempts/${attemptID}`, { headers: { Cookie: cookie } }, (socket) => {
    // Matches the real player's cadence (docs/05 section 5: "the attempt
    // WebSocket sends a heartbeat every 10s"); any frame counts server-side.
    socket.setInterval(() => socket.send("heartbeat"), HEARTBEAT_INTERVAL_S * 1000);

    let questionCursor = 0;
    if (questions.length > 0) {
      socket.setInterval(() => {
        const question = questions[questionCursor % questions.length];
        questionCursor++;
        const answerRes = http.put(
          `${BASE_URL}/api/v1/attempts/${attemptID}/answers/${question.id}`,
          JSON.stringify({ response: "a", time_spent_ms: AUTOSAVE_INTERVAL_S * 1000 }),
          { headers: { Cookie: cookie, "Content-Type": "application/json" }, tags: { name: "autosave" } },
        );
        autosaveMs.add(answerRes.timings.duration);
        check(answerRes, { "autosave succeeded": (r) => r.status === 200 });
      }, AUTOSAVE_INTERVAL_S * 1000);
    }

    socket.setTimeout(() => socket.close(), sessionDurationS * 1000);
  });
  // ws_connect_ok measures only the initial handshake (matches the docs'
  // "2,000 sockets" as a one-time connect target) - socket.on("error") fires
  // on any later mid-session hiccup too (e.g. the server closing an idle
  // connection), which would otherwise conflate transient session noise with
  // a failed connect and understate a clean handshake rate.
  const connected = !!res && res.status === 101;
  wsConnectOk.add(connected);
  check(res, { "ws upgraded": () => connected });
}

// exec.scenario.iterationInTest is unique and sequential within each named
// scenario (0-based), unlike exec.vu.idInTest which is a single counter
// shared across every scenario in the run - using it keeps each scenario's
// student-index range exactly [1, its own student count] regardless of
// which scenario's VUs happen to spin up first.
export function steadyStudent() {
  const index = 1 + exec.scenario.iterationInTest;
  runStudentSession(index, STEADY_SESSION_S);
}

export function herdStudent() {
  const index = STEADY_STUDENTS + 1 + (exec.scenario.iterationInTest % HERD_STUDENTS);
  runStudentSession(index, HERD_SESSION_S);
}
