/**
 * A MixerSnapshot built from the captured live frame, for demo.html only.
 *
 * Owner: WP-M4. GENERATED, then committed: derived from
 * internal/m2lx/testdata/switcher_status-live-2026-07-31.json, the real 84 KB
 * frame from the dev event, so the demo shows the 54 strips and
 * 7 buses an operator actually sees rather than a tidy invention.
 *
 * This is NOT the m2lx-to-snapshot conversion — that is WP-M2's, in Go. This is
 * a fixture, and nothing that ships imports it.
 *
 * Note what it contains: cam22-1, display name "CLAUDE-COMMS", routed to the
 * default ["master","aux1","aux2"] and therefore already in the clean feed.
 */

/** @type {import('./contract.js').MixerSnapshot} */
export const LIVE_FIXTURE = {
  "strips": [
    {
      "name": "cam1-1",
      "input": "cam1",
      "displayName": "Input 1",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam1"
      ],
      "subChMode": "ST_W",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -27.638126373291016,
        -28.059560775756836
      ],
      "peakHold": [
        -16.295732498168945,
        -15.198410034179688
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam1-2",
      "input": "cam1",
      "displayName": "Input 1",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam1"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam10-1",
      "input": "cam10",
      "displayName": "REPLAY 2 DIRTY",
      "muted": false,
      "follow": false,
      "followSources": [
        "cam10"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam10-2",
      "input": "cam10",
      "displayName": "REPLAY 2 DIRTY",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam10"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam14-1",
      "input": "cam14",
      "displayName": "Input 14",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam14"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam14-2",
      "input": "cam14",
      "displayName": "Input 14",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam14"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam15-1",
      "input": "cam15",
      "displayName": "Input 15",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam15"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam15-2",
      "input": "cam15",
      "displayName": "Input 15",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam15"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam16-1",
      "input": "cam16",
      "displayName": "Input 16",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam16"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam16-2",
      "input": "cam16",
      "displayName": "Input 16",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam16"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam17-1",
      "input": "cam17",
      "displayName": "Input 17",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam17"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam17-2",
      "input": "cam17",
      "displayName": "Input 17",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam17"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam18-1",
      "input": "cam18",
      "displayName": "Input 18",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam18"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam18-2",
      "input": "cam18",
      "displayName": "Input 18",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam18"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam19-1",
      "input": "cam19",
      "displayName": "Input 19",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam19"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam19-2",
      "input": "cam19",
      "displayName": "Input 19",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam19"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam2-1",
      "input": "cam2",
      "displayName": "Input 2",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam2"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        -0.4000000059604645,
        -0.4000000059604645
      ],
      "faderEnabled": [
        true,
        true
      ]
    },
    {
      "name": "cam2-2",
      "input": "cam2",
      "displayName": "Input 2",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam2"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam20-1",
      "input": "cam20",
      "displayName": "CLAUDE-TEST-SRT",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam20"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam20-2",
      "input": "cam20",
      "displayName": "CLAUDE-TEST-SRT",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam20"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam21-1",
      "input": "cam21",
      "displayName": "CLAUDE-FX",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam21"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam21-2",
      "input": "cam21",
      "displayName": "CLAUDE-FX",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam21"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam22-1",
      "input": "cam22",
      "displayName": "CLAUDE-COMMS",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam22"
      ],
      "subChMode": "ST_W",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -87.69413757324219,
        -87.65318298339844
      ],
      "peakHold": [
        -52.78746032714844,
        -52.646297454833984
      ],
      "metered": true,
      "fader": [
        -1.574803113937378,
        -1.574803113937378
      ],
      "faderEnabled": [
        true,
        true
      ]
    },
    {
      "name": "cam22-2",
      "input": "cam22",
      "displayName": "CLAUDE-COMMS",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam22"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam23-1",
      "input": "cam23",
      "displayName": "Input 23",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam23"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam23-2",
      "input": "cam23",
      "displayName": "Input 23",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam23"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam24-1",
      "input": "cam24",
      "displayName": "Input 24",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam24"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam24-2",
      "input": "cam24",
      "displayName": "Input 24",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam24"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam3-1",
      "input": "cam3",
      "displayName": "Input 3",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam3"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        true,
        true
      ]
    },
    {
      "name": "cam3-2",
      "input": "cam3",
      "displayName": "Input 3",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam3"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam4-1",
      "input": "cam4",
      "displayName": "Input 4",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam4"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        true,
        true
      ]
    },
    {
      "name": "cam4-2",
      "input": "cam4",
      "displayName": "Input 4",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam4"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam5-1",
      "input": "cam5",
      "displayName": "Input 5",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam5"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        -0.4000000059604645,
        -0.4000000059604645
      ],
      "faderEnabled": [
        true,
        true
      ]
    },
    {
      "name": "cam5-2",
      "input": "cam5",
      "displayName": "Input 5",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam5"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam6-1",
      "input": "cam6",
      "displayName": "Input 6",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam6"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        -0.4000000059604645,
        -0.4000000059604645
      ],
      "faderEnabled": [
        true,
        true
      ]
    },
    {
      "name": "cam6-2",
      "input": "cam6",
      "displayName": "Input 6",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam6"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam7-1",
      "input": "cam7",
      "displayName": "REPLAY 1 CLN",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam7"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        true,
        true
      ]
    },
    {
      "name": "cam7-2",
      "input": "cam7",
      "displayName": "REPLAY 1 CLN",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam7"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam8-1",
      "input": "cam8",
      "displayName": "REPLAY 2 CLN",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam8"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        true,
        true
      ]
    },
    {
      "name": "cam8-2",
      "input": "cam8",
      "displayName": "REPLAY 2 CLN",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam8"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam9-1",
      "input": "cam9",
      "displayName": "REPLAY 1 DIRTY",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam9"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "cam9-2",
      "input": "cam9",
      "displayName": "REPLAY 1 DIRTY",
      "muted": true,
      "follow": false,
      "followSources": [
        "cam9"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "MIC 1-1",
      "input": "MIC 1",
      "displayName": "MIC 1",
      "muted": true,
      "follow": false,
      "followSources": [
        "MIC 1"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "mon1"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "MIC 1-2",
      "input": "MIC 1",
      "displayName": "MIC 1",
      "muted": true,
      "follow": false,
      "followSources": [
        "MIC 1"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "mon1"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "MIC 2-1",
      "input": "MIC 2",
      "displayName": "MIC 2",
      "muted": true,
      "follow": false,
      "followSources": [
        "MIC 2"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "mon2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "MIC 2-2",
      "input": "MIC 2",
      "displayName": "MIC 2",
      "muted": true,
      "follow": false,
      "followSources": [
        "MIC 2"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "mon2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "MIC 3-1",
      "input": "MIC 3",
      "displayName": "CLAUDE-TEST-MIC",
      "muted": true,
      "follow": false,
      "followSources": [
        "MIC 3"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "mon3"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "MIC 3-2",
      "input": "MIC 3",
      "displayName": "CLAUDE-TEST-MIC",
      "muted": true,
      "follow": false,
      "followSources": [
        "MIC 3"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "mon3"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "replay1-1",
      "input": "replay1",
      "displayName": "Replay",
      "muted": true,
      "follow": false,
      "followSources": [
        "replay1"
      ],
      "subChMode": "ST_W",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -27.251710891723633,
        -26.94414520263672
      ],
      "peakHold": [
        -18.50044822692871,
        -16.878660202026367
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "replay1-2",
      "input": "replay1",
      "displayName": "Replay",
      "muted": true,
      "follow": false,
      "followSources": [
        "replay1"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "vtr1-1",
      "input": "vtr1",
      "displayName": "Clip Player 1",
      "muted": true,
      "follow": false,
      "followSources": [
        "vtr1"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "vtr1-2",
      "input": "vtr1",
      "displayName": "Clip Player 1",
      "muted": true,
      "follow": false,
      "followSources": [
        "vtr1"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "vtr2-1",
      "input": "vtr2",
      "displayName": "Clip Player 2",
      "muted": true,
      "follow": false,
      "followSources": [
        "vtr2"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    },
    {
      "name": "vtr2-2",
      "input": "vtr2",
      "displayName": "Clip Player 2",
      "muted": true,
      "follow": false,
      "followSources": [
        "vtr2"
      ],
      "subChMode": "MONO",
      "outputs": [
        "master",
        "aux1",
        "aux2"
      ],
      "pflOutputs": [],
      "level": [
        0,
        0
      ],
      "peakHold": [
        0,
        0
      ],
      "metered": false,
      "fader": [
        0,
        0
      ],
      "faderEnabled": [
        false,
        false
      ]
    }
  ],
  "buses": [
    {
      "name": "aux1",
      "muted": false,
      "channelCount": 2,
      "level": [
        -99.99999237060547,
        -99.99999237060547
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": 1,
      "faderPresent": true
    },
    {
      "name": "aux2",
      "muted": false,
      "channelCount": 2,
      "level": [
        -99.99999237060547,
        -99.99999237060547
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": 1,
      "faderPresent": true
    },
    {
      "name": "master",
      "muted": false,
      "channelCount": 2,
      "level": [
        -99.99999237060547,
        -99.99999237060547
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": 0,
      "faderPresent": true
    },
    {
      "name": "mon1",
      "muted": false,
      "channelCount": 2,
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": 0,
      "faderPresent": false
    },
    {
      "name": "mon2",
      "muted": false,
      "channelCount": 2,
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": 0,
      "faderPresent": false
    },
    {
      "name": "mon3",
      "muted": false,
      "channelCount": 2,
      "level": [
        -100,
        -100
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": 0,
      "faderPresent": false
    },
    {
      "name": "mon4",
      "muted": false,
      "channelCount": 2,
      "level": [
        -99.99999237060547,
        -99.99999237060547
      ],
      "peakHold": [
        -100,
        -100
      ],
      "metered": true,
      "fader": 0,
      "faderPresent": false
    }
  ],
  "takenAt": "2026-07-31T18:21:23.212Z"
};
