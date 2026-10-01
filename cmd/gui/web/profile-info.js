// Descriptions shown by the ⓘ button next to the benchmark profile
// selector, keyed by profile name. Text as written for each profile.
const PROFILE_INFO = {
  standard: `
<h4>Before the first test</h4>
<p>When you click Start, SwarmDialer checks that SERVERware is connected, that the VPSs are still on the tested host. It then:</p>
<ul>
  <li>reads each VPS's CPU and memory limits and RAM disk size, which the stop conditions and the report need</li>
  <li>reads the PBXware versions and licenses</li>
  <li>picks the active physical node/host</li>
</ul>

<h4>Each ramp test (tests 1 to 6)</h4>
<ol>
  <li><b>Set recording</b> on both instances, off, mono or stereo depending on the test, through the same API as the recording toggle.</li>
  <li><b>Prepare the trunks:</b> put the answering side's codec first in both trunks' codec lists. If that changes the order, wait up to 65 seconds for PBXware's reload cycle to apply it.</li>
  <li><b>1-minute idle baseline:</b> measure the host with no calls, so the report shows how much load the calls themselves add.</li>
  <li><b>Ramp up:</b> start 512 calls, one every 0.8 seconds, so reaching 512 takes about 7 minutes.
    <ul>
      <li>Why the calls last 10 minutes: the first calls must still be up when the last one starts, so the load really builds to 512.</li>
      <li>Why 512: the PBXware license limit.</li>
      <li>Why 0.8 seconds between calls: it's gradual enough that call setup itself isn't the stress being measured.</li>
    </ul>
  </li>
  <li><b>Hold for 1 minute at 512 calls:</b> the steady-state measurement that the report's "at target" figures come from.</li>
  <li><b>Stop:</b> hang up all calls.</li>
  <li><b>2-minute cooldown:</b> still measured, because PBXware's MP3 conversion and any backlog show up here, and it lets the host return to idle before the next test.</li>
  <li><b>MOS lookup:</b> read PBXware's call quality score for up to 200 of the test's calls.</li>
</ol>

<p><b>What stops a test early</b>, all measured every 5 seconds:</p>
<ul>
  <li>host CPU or memory at 95% or more for 15 seconds</li>
  <li>a VPS reaching its own CPU or memory limit</li>
  <li>SwarmDialer itself over 80% CPU, which marks the result invalid</li>
</ul>
<p>The test then records how many calls ran at once, and why it stopped.</p>

<p><b>What's recorded without stopping the test:</b></p>
<ul>
  <li>the call count where quality first dropped: more than 1% of calls failing, or p95 setup time over 500ms</li>
  <li>for recording tests, the estimated point where the RAM disk fills, and the MP3 conversion delay</li>
</ul>

<h4>The six ramp tests</h4>
<table>
  <tr><th>#</th><th>Recording</th><th>Codec (caller → callee)</th><th>What it loads</th></tr>
  <tr><td>1</td><td>none</td><td>ulaw → ulaw</td><td>Baseline cost of a call: SIP signaling, routing, and forwarding audio packets. PBXware never touches the audio itself</td></tr>
  <tr><td>2</td><td>none</td><td>opus → ulaw</td><td>Adds transcoding: PBXware decodes and re-encodes every call's audio in both directions</td></tr>
  <tr><td>3</td><td>mono</td><td>ulaw → ulaw</td><td>Adds recording: decoding both directions, mixing them into one track, writing wav49 to the RAM disk, then converting to MP3 after each call</td></tr>
  <tr><td>4</td><td>mono</td><td>opus → ulaw</td><td>Transcoding plus recording</td></tr>
  <tr><td>5</td><td>stereo</td><td>ulaw → ulaw</td><td>Stereo recording: two tracks at larger sizes. This fills the RAM disk fast (about 29KB per second per call), so expect "RAM disk full" at roughly 35 to 60 calls</td></tr>
  <tr><td>6</td><td>stereo</td><td>opus → ulaw</td><td>The heaviest combination</td></tr>
</table>
<p>The tests are ordered so each adds one kind of work to the previous. Comparing them separates the cost of each: transcoding (2 minus 1), recording (3 minus 1), and stereo compared with mono (5 minus 3).</p>

<h4>Test 7: rolling stereo</h4>
<ul>
  <li><b>How it works:</b> stereo recording, ulaw, 1-minute calls, each replaced as soon as it ends, at 2, then 4, 6 and 8.5 new calls per second, 90 seconds per rate. At 8.5 calls per second, about 510 calls run at once.</li>
  <li><b>What it measures:</b> unlike the ramp tests, calls end continuously, so PBXware converts about 8.5 recordings to MP3 every second while also recording and setting up about 8.5 new calls per second. The key figure is the MP3 conversion delay, and whether it grows over the test, which means the host is falling behind.</li>
</ul>

<h4>After the last test</h4>
<p>Recording is turned off, the full results are saved to runs/, and the report is built, saved to reports/, and uploaded to DT Collector.</p>

<h4>Why opus for the high-cost tests?</h4>
<p>The high-cost tests only make sense if PBXware has to transcode. When both sides use the same codec, PBXware just forwards the packets whatever the codec is, so even "expensive" codecs cost it nothing. Codec choice alone doesn't load PBXware.</p>
<p>Of the codecs SwarmDialer supports (ulaw, alaw, G.722, opus), opus is the most expensive to transcode:</p>
<ul>
  <li>it's a complex modern codec</li>
  <li>it runs at 48 kHz, so converting to ulaw at 8 kHz also means resampling</li>
  <li>G.722 costs less, and ulaw ↔ alaw costs almost nothing</li>
</ul>
<p>It showed clearly in testing: 10 opus → ulaw calls took MT's Asterisk to about 63 to 83% of one CPU core, against about 9% for 10 plain ulaw calls. It's also realistic, because browsers and mobile apps typically use opus while trunks and desk phones use G.711.</p>

<h4>Two limitations to know about</h4>
<ol>
  <li>In the high-cost tests, transcoding happens on MT only. The trunk leg is ulaw, so MT converts opus ↔ ulaw and CC just forwards ulaw. Those tests load MT much more than CC. That's fine, since we measure the whole host, but the per-VPS figures will be lopsided.</li>
  <li>The audio is synthetic speech, not real conversations. Each call sends a 30-second loop of speech-like audio (talk spurts and pauses, about 55% talk, with a low background noise floor), so PBXware decodes, transcodes, records and converts audio shaped like one side of a conversation rather than digital silence, which codecs process more cheaply. The audio is encoded once when SwarmDialer starts and shared by every call, so SwarmDialer itself stays light, like the remote phones it stands in for. Its opus is CELT-mode at 8 kHz, while many phones and apps use opus's SILK or hybrid modes, so the transcoding cost can still differ somewhat from real devices. The comparison between hosts is fair, because every host gets exactly the same audio.</li>
</ol>`,

  smoke: `
<p>The smoke profile is a 5-minute check that the whole pipeline works, not a measurement. It runs the same engine, stop conditions and report steps as the standard script, at a scale too small to load the host. Use it after setting up a new deployment, or after changes, before starting the 77-minute standard run.</p>

<h4>The three tests, and what each one checks</h4>
<ol>
  <li><b>ramp_norec_low:</b> 10 calls, ulaw → ulaw, no recording. Checks the basic path:
    <ul>
      <li>setting recording off</li>
      <li>the trunk codec check</li>
      <li>baseline, ramp, hold, stop and cooldown</li>
      <li>sampling the host, VPSs and SwarmDialer through Prometheus</li>
      <li>the MOS lookup</li>
    </ul>
  </li>
  <li><b>ramp_stereo_high:</b> 10 calls, opus → ulaw, stereo recording. Checks everything a heavy test depends on:
    <ul>
      <li>that transcoding works (the answering side uses a different codec; the earlier smoke runs showed MT's Asterisk jumping to about 60% of a core)</li>
      <li>switching on stereo recording on both instances</li>
      <li>the RAM disk estimate</li>
      <li>the MP3 conversion delay, polled from the CDRs</li>
    </ul>
  </li>
  <li><b>rolling_stereo:</b> 20-second calls, stereo recording, at 0.5 and then 1 new call per second, 30 seconds each (up to about 20 calls at once). Checks the rolling mode:
    <ul>
      <li>placing calls continuously in chunks, at a set rate</li>
      <li>moving between rate steps</li>
      <li>measuring the conversion delay while calls end continuously</li>
    </ul>
    <p>Like the standard rolling test, only calls that ran their full length count toward the delay figures.</p>
  </li>
</ol>
<p>When it finishes, recording is turned off, the results are saved to runs/, and a report is built and saved to reports/. That covers building the report, checking it for secrets, and saving it. It isn't uploaded: the numbers from 10 or 20 calls say nothing about a host's capacity, and uploading them would clutter DT Collector's comparisons. The upload step itself is only exercised by a real standard run, or by Retry upload on a standard report.</p>

<h4>What a passing smoke run tells you</h4>
<ul>
  <li>all three tests end with "Target reached"</li>
  <li>calls are answered with none failed</li>
  <li>MOS and the MP3 delay have values</li>
  <li>the report panel says "Saved locally"</li>
</ul>
<p>If so, SERVERware, both PBXware instances, the trunk and the recording settings are all working, and the standard run should go through.</p>`,
};
