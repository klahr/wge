# On-call handover

- agent alerts at 02:00 if the nightly did not stage
- if staging is missing, check disk before anything else
- do NOT restart the agent while a restore is open, it will half-write
- escalation: r.vahl, then drift
