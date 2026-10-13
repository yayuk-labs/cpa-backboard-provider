# Management page design

The Backboard key page extends the existing CPA Zed plugin management-page pattern. It is an operational form for CPA administrators, with a single column and explicit field labels. Key entry and model settings have separate save actions. The model editor uses provider, model name, and optional alias rows; rows stack on mobile. Load current settings before editing, preserve input on failed saves, and explain empty-list and full-discovery behavior.

Use system UI typography at 16px, a 38rem maximum content width, 1.5rem side padding, and visible keyboard focus. Inputs use 6px corners and 1px borders. The primary action uses #216b42 with white text; hover uses #195432. Light surfaces use #f5f5f3 and #202321, with #515952 secondary text. Dark surfaces follow the user's system preference with #181c19 backgrounds, #edf1ed text, #242b26 fields, and #bbc5bd secondary and placeholder text.

Keep authentication fields masked. Use a live status message for loading, errors, duplicates, and success. Disable the form during saving, clear only the submitted Backboard key on success, and preserve entered data on errors. Use no browser storage, decorative imagery, external fonts, or UI dependencies.

Verified in Chromium at desktop 1280px and mobile 390px, including light/dark themes, keyboard-ready labels, layout overflow, authenticated save, rejection, duplicate preservation, and key clearing.
