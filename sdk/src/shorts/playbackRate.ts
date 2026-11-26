/** 长按临时加速结束后，恢复用户在倍速菜单选择的速率。 */
export function restoreShortsPlaybackRate(video: HTMLVideoElement) {
  const rate = video.defaultPlaybackRate;
  video.playbackRate = Number.isFinite(rate) && rate > 0 ? rate : 1;
}
