export type VideoItem = {
  id: string;
  href: string;
  title: string;
  thumbnail: string;
  previewSrc: string;
  previewDuration: number;
  previewStrategy: "teaser-file" | "sprite-frames";
  duration: string;
  badges: string[];
  sourceLabel?: string;
  author: string;
  views: number;
  favorites?: number;
  comments?: number;
  likes?: number;
  dislikes?: number;
  publishedAt: string;
  rating?: number;
  tags?: string[];
};

export type AuthorProfile = {
  id: string;
  name: string;
  href: string;
  badges: string[];
  signupAge?: string;
  level?: number;
  points?: number;
  videoCount?: number;
  followers?: number;
  following?: number;
  isFollowing?: boolean;
};

export type CommentItem = {
  id: string;
  author: string;
  body: string;
  createdAt: string;
  likes?: number;
};

export type VideoCollectionSummary = {
  name: string;
  total: number;
  /** One-based position in the canonical ascending directory order. */
  currentIndex: number;
};

export type VideoCollectionItem = Pick<
  VideoItem,
  "id" | "href" | "title" | "thumbnail" | "duration" | "views" | "publishedAt"
> & {
  /** Present only when the desktop collection requests preview metadata. */
  previewSrc?: string;
};

export type VideoCollection = VideoCollectionSummary & {
  items: VideoCollectionItem[];
};

export type VideoDetail = VideoItem & {
  videoSrc: string;
  /** 实际交给浏览器播放的资源 MIME；后端无法确认时省略。 */
  mediaType?: string;
  poster: string;
  description: string;
  embedUrl: string;
  points?: number;
  authorProfile: AuthorProfile;
  collection?: VideoCollectionSummary;
  relatedVideos: VideoItem[];
  commentsList: CommentItem[];
};

export type VideoSubtitle = {
  name: string;
  label: string;
  language?: string;
  ext: string;
  type: "vtt" | "srt" | "ass";
  url: string;
  source: string;
};

export type PreviewState = "idle" | "intent" | "loading" | "playing" | "error";

export type SortKey = "latest" | "hot" | "recent";

export type TagItem = {
  id: string;
  label: string;
  count?: number;
};

export type PromoItem = {
  id: string;
  kind: "channel" | "topic" | "event";
  label: string;
  title: string;
  meta?: string;
};
