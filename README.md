# Miranda Medical Card

**A family electronic health record that reads your lab results, discharge summaries, and prescriptions itself — and answers questions about your health in plain language.**

This is a module of [Miranda](https://github.com/archer-developer/miranda) — a home voice assistant. Miranda itself knows nothing about medicine: it talks, listens, and remembers context. Anything health-related, it hands off to this module — its "medical expert" — and reads the finished answer back to you.

```
   You tell Miranda                  Miranda            The medical module answers
   "What's my cholesterol?"    ───▶  forwards  ───▶      using your real documents
                                      the question         and health history
   Voice / Telegram / chat     ◀──────────────────────
```

---

## The problem it solves

A family's medical history usually lives in three places at once: a folder of paper lab results, photos buried in a phone's gallery, and memory that forgets dates and dosages. When a doctor asks "what were you prescribed in March?" — you're left digging through old clinic records or trying to recall it from memory.

Miranda Medical Card turns that into one spoken question: *"What medications was I taking last winter?"* — and gets back a precise answer, with a link to the exact document it came from.

---

## What it can do

### 📄 Reads your documents for you
Send it a photo of a lab result, a hospital discharge summary, or a prescription — it recognizes the text itself, pulls out diagnoses, medications, procedures, lab values, dates, and doctors, and folds all of it into the bigger picture of your health. The original file is never altered or lost — every conclusion can be recomputed from scratch if needed.

### 💬 Answers questions about your health
The core feature is `medical.ask`. Ask it the way you'd ask a person, not a search engine:

> — When did my ALT first go up?
> — According to a blood test from March 12, 2025 — the level was 54.7, against a normal range up to 40.

> — Why did the doctor stop my statins?
> — Per the discharge note from June 20: the medication was discontinued due to a side effect — muscle pain.

> — What medications am I currently taking?
> — Rosuvastatin 10 mg and Losartan 50 mg, both under active prescriptions.

It never makes up facts: if the data isn't there, it says so — "there's no information about this in your history." And it doesn't replace a doctor — for anything that requires real diagnosis, it says so explicitly.

### 🕒 A medical timeline
Every event — tests, visits, prescriptions, surgeries — is laid out in chronological order. Ask "what happened after the surgery?" and get an actual sequence of events, not a pile of unrelated documents.

### 🗂 A health card in one request
An aggregated snapshot of your current state: active diagnoses, chronic conditions, medications you're taking right now, known allergies, latest lab values, past surgeries, vaccinations. No analysis — just an up-to-date snapshot, like the card at a doctor's office.

### ⏰ Reminders for what's coming up
If a discharge note says "repeat this test in six months," it remembers — and can hand back a list of what's due and when. Miranda turns that into an actual reminder.

### 🗣 Logs events by voice, no document needed
Not everything comes with paperwork. Just say "I started taking vitamin D" or "got my flu shot" and it records it as a standalone event — honestly labeled as something you reported yourself, not something a document confirmed.

### 🥗 Nutrition advice that accounts for your diagnoses
It knows your diagnoses and allergies — and can give dietary advice that actually takes them into account, instead of generic "eat more vegetables" tips.

### 👨‍👩‍👧 One shared record for the whole family
Every family member has their own history. Parents can see their children's data (with permission), never the other way around. Each person has their own documents, their own medications, their own timeline.

### 🔒 Private by default
Health data is the most sensitive thing in the house. Encryption can be turned on per person: document contents and history become unreadable without a key — one that isn't stored in this module at all, but in Miranda itself, unlocked only by your password or Face ID/fingerprint.

---

## A sample conversation

> — Do I have any liver problems?
> — Your history shows elevated ALT and AST in March 2025. Levels gradually dropped after statins were discontinued in June. There are no more recent liver tests — worth asking your doctor whether follow-up monitoring is needed.

> — What changed after starting statins?
> — A month after starting (May 14, 2025), liver values were elevated, which is what led to the medication being discontinued on June 20.

Every answer like this comes with links to the documents it was built from — so you can open the original and check it yourself.

---

## How it works under the hood (short version)

- Runs as a separate service that Miranda talks to over the MCP protocol — so the module itself knows nothing about Telegram, the web UI, or smart speakers, only about medical data.
- Documents are processed once, at upload time: text recognition → structured data extraction → the timeline and health card get updated.
- Questions are handled by an LLM agent that decides for itself which data sources it needs to answer (history, medications, lab results, documents) — no hardcoded scripts.
- A single static binary, no Docker, self-hosted on your own server — medical data never leaves to someone else's cloud.

Full technical documentation lives under `docs/` (architecture, domain model, MCP tool contracts).

---

## Being upfront about what it doesn't do

- It doesn't replace a doctor or diagnose things from scratch — it works with what's already in your documents.
- It doesn't recommend specific drugs or dosages.
- It doesn't fetch files on its own — a document has to be uploaded by the user, or by Miranda on the user's request.

---

## License

Free for noncommercial use — at home, for your family, to study and modify. Commercial use requires a separate license. Details in [LICENSE](LICENSE).
