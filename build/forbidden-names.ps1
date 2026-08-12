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
# it). The rest are in the same family: GPL, or patent-encumbered, or both, and
# none of them has any business in this pipeline.

$ForbiddenPatterns = @(
    '*x264*'        # GPL-2.0-or-later. THE reason this control exists.
    '*x265*'        # GPL-2.0-or-later.
    '*libav*'       # gst-libav / FFmpeg: licence depends on how it was built. Not ours to assume.
    '*ffmpeg*'      # as above.
    '*avcodec*'     # FFmpeg component.
    '*avformat*'    # FFmpeg component.
    '*avfilter*'    # FFmpeg component.
    '*postproc*'    # FFmpeg component, GPL.
    '*swscale*'     # FFmpeg component.
    '*swresample*'  # FFmpeg component.
    '*ugly*'        # gst-plugins-ugly: the set exists precisely because of licensing.
    '*faac*'        # patent-encumbered AAC encoder. We use mfaacenc (the OS).
    '*lame*'        # LGPL but patent-encumbered MP3; nothing here needs MP3.
    '*mpeg2enc*'    # GPL.
    '*a52dec*'      # GPL.
    '*dvdread*'     # GPL.
    '*libmad*'      # GPL.
)
