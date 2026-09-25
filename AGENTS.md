# Guidelines

1. Always attempt to test, run and simulate as much as possible locally where it's possible.
2. Avoid sequential long time-consuming runs and instead try where possible to parallelize and be time-efficient.
3. If a permission has been granted, it has been granted for the entire session, and you do not have to ask again.
4. Avoid preserving or adding: legacy code, legacy adapters, legacy features, legacy behaviour, docs that mention legacy features / old relics; instead aim for migrating and deleting these. If you're unsure about if something is intenially preserved --> **ask**.
5. In general: Let me know if anything is unclear or if you have any questions, but not for the sake of doing so. 


## Rules:
1. Max 1 sentence per comment (//, #, /* */, ...etc...)
2. **Only** add a comment if **absolutely necessary**
3. Comments and descriptions should be **stateless** (E.g; No references to a plan or decision)
4. Do **not** inline a comment inside the code unless **absolutely necessary**, and max 1 sentence (E.g; **No** `description="...."`)
5. The code itself should be readable and understandable with as **little** context as possible.