# Assets

Illustrations referenced from the documentation.

`readme/` holds the root README's:
- `hero.jpg`, and `social-preview.jpg`, the hero cropped to 1280×640 for the
  repository's social preview;
- `architecture.png`, the system;
- `golden-path.gif`, the golden path's ten stations;
- `reconciliation.png`, the three-way reconciliation, also in
  [`docs/reconciliation.md`](../reconciliation.md).

`simulators/` holds one diagram per simulator README: what Jupiter sends it, and what it
answers.

They were generated with Higgsfield (GPT Image 2 for the stills, Seedance 2.0 for the
animation), from prompts that state every label. Each was checked against the code before
it was committed. They are drawings, not evidence: the evidence is the tests, the
simulations and the benchmarks. When a module, a step of the golden path or a
simulator's protocol changes, regenerate the drawing or remove it.
