# Readiness Engineer — Build Project

## At a glance

You'll build a small production-grade system: it prepares tax forms (1099s) in bulk for accounting firms and files them with the IRS.

- Stack: Ruby on Rails is what we run day to day, so there's a slight preference for it — but strong backend engineering in the stack you know best is what matters. Database and job backend are your call.
- Deliverables: a new GitHub repo shared with @seanmcoleman, a short video walkthrough, and a write-up. We run everything locally — no deployment.
- Make reasonable assumptions and document decisions.

## Why this problem

Soraban builds workflow automation for accounting firms. Our industry is seasonal (most of a firm's revenue lands between January and mid-April), trust-heavy (regulated client data, professional liability), and unforgiving (a mistake is a penalty notice, not a bug report).

Our engineering team splits product work into two roles.

Discovery Engineers own figuring out what to ship: they work with product to turn rough ideas into working prototypes, put them in front of users, and prove what's worth building.

Readiness Engineers own making it real: the architecture, data models, performance, security, and failure recovery that turn a validated prototype into something every firm can rely on during tax season.

Discovery answers "should we build this?"; Readiness answers "will this hold up?" This project is for the Readiness Engineer role. It's a compressed version of that job: the hard part isn't the features, it's making them correct under failure.

## The problem

In the US, when a business pays an independent contractor $600 or more in a year, it has to report those payments: the contractor gets a tax form — a 1099-NEC — and a copy is filed with the IRS. Most businesses don't handle this themselves; their accounting firm prepares and files on their behalf, using filing credentials the IRS issues to the firm. (Everything you need to know about the rules is spelled out in Part 2 — no tax background required.)

Work in tax year 2025, filed January 2026. Because January 31, 2026 falls on a Saturday, the deadline is Monday, February 2, 2026.

In production, a firm files for thousands of business clients at once, from millions of payment records, mostly in the last few days before the deadline, through one shared, rate-limited channel to the IRS. A filing run takes hours, and nobody is watching it the whole time. So the bar is simple: a run can be started, crash or be interrupted partway through, be resumed, and finish correctly — and whenever someone checks on it, the status they see is the truth. You'll build at a smaller scale, but every design decision should hold up under that bar.

## Build this

Four parts. Part 3 is what we're primarily evaluating.

---

### Part 1 — Import

We provide the data — you don't generate it. Two files, import both:

| File                    | What it is                                                                             |
| ----------------------- | -------------------------------------------------------------------------------------- |
| firm_F001_export.csv.gz | Firm F001: ~500,000 payment records across ~250 business clients (clients C0001–C0250) |
| firm_F002_export.csv.gz | Firm F002: ~500,000 payment records across ~250 business clients (clients C0251–C0500) |

Each file is a year of payments that the firm's clients made to their vendors, recorded however each client's bookkeeper felt like recording it. Vendor names are inconsistent, tax IDs are sometimes missing, and negative amounts are reversals. Nothing in the data identifies a real person or business — every name, ID, and amount is generated.

Schema — CSV, UTF-8, header row:

| Column             | Meaning                                                                                                                                                                     |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| client_id          | The firm's client this line belongs to (e.g. C0042)                                                                                                                         |
| vendor_name        | Vendor name as the bookkeeper recorded it. Spelling is not consistent — the same vendor may appear under several variants.                                                  |
| vendor_tin         | The vendor's taxpayer identification number ("TIN") — their tax ID. For an individual, this is their Social Security number. May be blank if the client never collected it. |
| payment_date       | ISO date within tax year 2025                                                                                                                                               |
| amount             | Payment amount in USD. Negative amounts are reversals/refunds.                                                                                                              |
| payment_method     | One of: check, ach, wire, cash, credit_card, paypal                                                                                                                         |
| backup_withholding | Tax withheld from this payment and sent directly to the IRS (called "backup withholding"). Almost always 0.00.                                                              |
| memo               | Free text, often empty                                                                                                                                                      |

Requirements:

- A full firm export imports in well under two minutes.
- Importing the same file twice leaves the system in exactly the state that importing it once did. Show us how you verify that.
- An import that fails or is interrupted partway can be retried and completes correctly — no duplicated records, no lost records.
- Memory use stays roughly flat as file size grows — a file several times this size shouldn't need several times the memory.
- One firm's import never touches the other firm's data.

---

### Part 2 — Determination

For each business client, determine which vendors require a 1099-NEC — and make it explainable: for any vendor, the system can show which payments counted, which didn't and why, and the total.

The rules you need — no tax background required, apply these:

- A 1099-NEC is required for any vendor paid $600 or more during the tax year for services.
- Vendors are identified by TIN, not by name string. Payments under different spellings of the same vendor's name with one TIN aggregate to one vendor.
- The reportable amount is the net paid for the year — reversals and refunds reduce it.
- "$600 or more" is inclusive: a total of exactly $600.00 requires a form.
- Payments made by credit card or third-party payment networks don't count — the payment processor reports those to the IRS separately. Only the non-card portion counts toward the threshold.
- If backup withholding was taken from a vendor, a 1099-NEC is required regardless of amount, and it reports the withholding.
- A missing TIN doesn't remove the filing obligation. The vendor still requires a form, but it can't transmit cleanly — treat it as an exception for a human to resolve (someone needs to collect the vendor's tax ID), never a reason to silently skip the vendor.

The provided data includes each of these six situations. Your system must handle them correctly:

1. The same vendor under three spellings of their name, one TIN.
2. A December payment reversed: gross for the year $800, net $250.
3. A vendor total of exactly $600.00.
4. A vendor with no TIN (no tax ID on file).
5. $2,400 paid to a vendor, $1,900 of it by credit card.
6. $400 paid to a vendor, with backup withholding taken.

A full determination pass across the entire provided dataset should complete in under a minute.

---

### Part 3 — Transmission (the heart of this project)

Filings go to the IRS in batches. There's no IRS sandbox, so you'll build a stub that behaves like production. The stub itself is small and isn't what we're evaluating — how your system behaves against it is.

Your stub's behavior:

| Behavior         | Specification                                                                                                                                                                                                                                                                                                                                                                                                          |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Submission call  | At most 100 filings, all for the same client. Accepts a caller-supplied reference string that uniquely identifies the submission attempt.                                                                                                                                                                                                                                                                              |
| Success response | Returns a submission ID — an opaque unique string identifying that submission. That is all it returns: no per-filing results yet.                                                                                                                                                                                                                                                                                      |
| Failure mode A   | Some submission calls fail before anything is recorded. Failures are random; roughly 7% is a rough estimate, not a fixed rate. Retrying is safe.                                                                                                                                                                                                                                                                       |
| Failure mode B   | Some submission calls record every filing, then return an error anyway. Also random; roughly 5%, again a rough estimate. The filings are live at the IRS, and you never see the submission ID                                                                                                                                                                                                                          |
| Status call      | A separate call retrieves what happened to a previous submission. Look it up by its submission ID — or by the reference string you supplied when submitting, which works even if the original call errored and you never received an ID. Acknowledgment arrives after a configurable delay (default it to ~10–30 seconds; in production it's minutes to hours, occasionally never — your design shouldn't care which). |
| Acknowledgment   | Per filing, not per batch. Each filing is either accepted — and gets a record identifier — or rejected with exactly one of these reason codes. This list is exhaustive: TIN_MISSING (no TIN on the filing), TIN_MALFORMED (TIN is not nine digits), TIN_INVALID (TIN begins with 000), AMOUNT_INVALID (amount is zero or negative).                                                                                    |
| Rate budget      | 20 calls per rolling 60 seconds per firm, shared across all clients and both call types. Excess calls refused.                                                                                                                                                                                                                                                                                                         |

Requirements:

- Zero duplicate filings, ever — across crashes, restarts, retries, and especially failure mode B. How you achieve that is yours to design.
- Kill a run mid-batch and resume it. Write the test that proves the recovered state is right. This test is required.
- Never exceed the rate budget.

---

### Part 4 — Status view

At any point — while a filing run is in progress, after it finishes, or after it crashed and was resumed — a staff member can see at a glance:

- Per-client status: fully filed / partially filed / awaiting the IRS / needs attention.
- An exception list of everything needing a person, grouped by type: vendor with no TIN, filing rejected (with reason), submission unacknowledged too long, anything else your state model produces.

Build a very light web front-end for this — enough to click around, kick off actions, and demo the system in your video. To be clear: the UI itself is not graded. You don't need to spend time on visual design or polish; it's a tool for demoing and using what you built, and what we're evaluating is whether the information it shows is fast and truthful.

---

## Explicitly out of scope

Don't build these. Some appear in the write-up instead.

- Corrections and revised exports. In reality, books change after filings go out (a bookkeeper finds a missed invoice), and fixing an accepted filing means a corrected form to the IRS and the vendor, penalties, and a permanent record alongside the original.
- Authentication, account management, notifications — stub or omit.
- Client approval flow — assume filings arrive approved.

## What to submit

1. Repository — read access to @seanmcoleman. README covers setup, how to import the provided data, how to run a filing run, and your filing state model. Include a task or log output that reports import and determination timings. Assume a normal dev environment and nothing else.
2. Video — about 5 minutes, screen-sharing the running app and the code. We're more interested in why than what.

## On AI tools

Use whatever you'd use on the job, including AI — that's how we work. But anything you ship, you own: we'll ask you to extend it, in detail, in a later conversation.
