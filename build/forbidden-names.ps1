# Shared licensing control: file names that must never appear in anything this
# project redistributes.
#
# Dot-sourced by build\bundle-gst.ps1, which stages the GStreamer bundle, and by
# build\pack-portable.ps1, which packs that bundle into a single executable.
# Both redistribute the same binaries, so both must apply the same list - and a
# security-relevant list kept in two places is a list that eventually differs in
# two places. It lives here so there is exactly one of it.
#
# A match is fatal in both callers, always, with no override switch. There is
# deliberately no -Force anywhere near this.
#
# x264 is the one the specification names (GPL-2.0-or-later; mfh264enc replaces
# it) and it stays forbidden, as do x265, the GPL/patent-encumbered audio codecs
# and gst-plugins-ugly. gst-libav / FFmpeg is the ONE deliberate exception,
# admitted 2026-08-24 as the picture's software decode fallback; see the note in
# the list below.

$ForbiddenPatterns = @(
    '*x264*'        # GPL-2.0-or-later. THE reason this control exists.
    '*x265*'        # GPL-2.0-or-later.
    # gst-libav / FFmpeg was ADMITTED on 2026-08-24 as the picture's software
    # HEVC/H.264 decode fallback, for Windows machines whose GPU exposes no
    # hardware decode profile so d3d11h265dec never registers. The owner took
    # that decision with the licence and the HEVC-patent posture understood and
    # accepted for this internal deployment. So *libav*, *ffmpeg*, *avcodec*,
    # *avformat*, *avfilter*, *swscale* and *swresample* are NO LONGER forbidden:
    # the seven files libgstlibav.dll needs are named one by one in
    # bundle-gst.ps1's allowlist, which is the control that still governs exactly
    # what ships. See internal/gst/picture_cgo.go's "libav is the software
    # fallback" header and NOTICE.txt.
    '*postproc*'    # FFmpeg's libpostproc STAYS forbidden: it is GPL, and nothing
                    # in the decode path needs it - it is not in libgstlibav.dll's
                    # dependency closure (verified by objdump, 2026-08-24).
    '*ugly*'        # gst-plugins-ugly: the set exists precisely because of licensing.
    '*faac*'        # patent-encumbered AAC encoder. We use the OS: mfaacenc / atenc.
    '*faad*'        # GPL AAC DECODER (gst-plugins-bad). Not a typo for faac above:
                    # different project, different licence, one letter apart, and
                    # only the encoder was on this list until macOS arrived. The
                    # decoder is atdec there and mfaacdec on Windows - both OS.
    '*fdkaac*'      # Fraunhofer FDK AAC. Measured indistinguishable from atenc on
                    # rate (131.4 vs 131.4 kbit/s against a 128k target) and excluded
                    # anyway, on licence. Two commonly consulted sources are WRONG
                    # about it: Homebrew's formula metadata says Apache-2.0 and
                    # gst-inspect says LGPL - the latter describing the GStreamer
                    # wrapper, not the libfdk-aac it links. The real terms grant NO
                    # PATENT LICENCE (clause 3) and require source availability for
                    # binary redistribution. See NOTICE.txt section G.
    '*lame*'        # LGPL but patent-encumbered MP3; nothing here needs MP3.
    '*mpeg2enc*'    # GPL.
    '*a52dec*'      # GPL.
    '*dvdread*'     # GPL.
    '*libmad*'      # GPL.
)
