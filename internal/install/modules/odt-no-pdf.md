---
title: "Never convert ODT to PDF unasked"
group: Personal workflow
order: 210
description: "Leave editable documents for the user to convert after review."
---

### 📄 Never convert ODT to PDF without explicit permission

**NEVER convert an `.odt` (or any editable source document) to PDF unless the user explicitly
asks in that message.** Applies to resumes (`~/Documents/*Resume*.odt`,
`~/Documents/resumes/*.odt`), cover letters, and any ODT-authored document.

- Do **not** run `soffice` / `libreoffice` `--convert-to pdf`, `unoconv`, `docx2pdf`, or any
  equivalent as a "finishing step."
- Do **not** convert to verify page count, layout, or formatting. To check length, reason about
  content volume relative to a known-good prior version — and say that's what you did.
- **Why:** the user reviews the ODT by hand first, adjusting wording, spacing, and page breaks,
  so the PDF meets their expectations before it becomes the artifact that goes to an employer.
  An agent-generated PDF is premature and can be mistaken for the final, reviewed version.
- **How to apply:** finish the ODT, say it's ready and where it lives, let them convert. Permission
  given in a turn covers that one conversion, not future ones.
