import {
  Suspense,
  lazy,
  useEffect,
  useLayoutEffect,
  useRef,
  type ReactNode,
} from "react";
import {
  Navigate,
  Route,
  Routes,
  useLocation,
  useMatch,
  useNavigationType,
  type Location,
} from "react-router";
import { SkyStarfield } from "@/components/SkyStarfield";
import { VideoDetailLoading } from "@/components/VideoDetailLoading";
import { CrawlersPageLoading } from "@/admin/CrawlersPageLoading";
import { DrivesPageLoading } from "@/admin/DrivesPageLoading";
import { useAuth } from "@/admin/AuthContext";
import { RequireAuth } from "@/admin/RequireAuth";
import { RequireAdmin } from "@/admin/RequireAdmin";
import {
  loadBackupPage,
  loadCrawlersPage,
  loadDrivesPage,
  loadLogsPage,
  loadSettingsPage,
  loadTelegramPage,
  loadTagsPage,
  loadUsersPage,
  loadVideosPage,
} from "@/admin/adminPageModules";
import { loadVideoDetailPage } from "@/lib/videoDetailRoute";
import {
  PageScrollRootProvider,
  scrollPageTo,
  usePageScrollRoot,
} from "@/lib/pageScroll";
import { watchPreviewSettings } from "@/lib/previewSettings";
import { RouteActivityProvider } from "@/lib/routeActivity";
import { useDocumentScrollLock } from "@/lib/useDocumentScrollLock";
import { useNativeBackNavigation } from "@/lib/useNativeBack";
import { rememberVideoReturnPath, routeToPath } from "@/lib/videoReturnPath";
import {
  isVideoListingPath,
  readVideoListingBackground,
} from "@/lib/videoListingBackground";

const HomePage = lazy(() => import("@/pages/HomePage"));
const ListingPage = lazy(() => import("@/pages/ListingPage"));
const ShortsPage = lazy(() => import("@/pages/ShortsPage"));
const UploadPage = lazy(() => import("@/pages/UploadPage"));
const VideoDetailPage = lazy(loadVideoDetailPage);
const SharedVideoPage = lazy(() => import("@/pages/SharedVideoPage"));

const AdminLayout = lazy(() =>
  import("@/admin/AdminLayout").then((module) => ({
    default: module.AdminLayout,
  }))
);
const LoginPage = lazy(() =>
  import("@/admin/LoginPage").then((module) => ({ default: module.LoginPage }))
);
const DrivesPage = lazy(() =>
  loadDrivesPage().then((module) => ({ default: module.DrivesPage }))
);
const CrawlersPage = lazy(() =>
  loadCrawlersPage().then((module) => ({ default: module.CrawlersPage }))
);
const VideosPage = lazy(() =>
  loadVideosPage().then((module) => ({ default: module.VideosPage }))
);
const TagsPage = lazy(() =>
  loadTagsPage().then((module) => ({ default: module.TagsPage }))
);
const TelegramPage = lazy(() => loadTelegramPage().then(module => ({default: module.TelegramPage})));
const SettingsPage = lazy(() =>
  loadSettingsPage().then((module) => ({ default: module.SettingsPage }))
);
const BackupPage = lazy(() =>
  loadBackupPage().then((module) => ({ default: module.BackupPage }))
);
const UsersPage = lazy(() =>
  loadUsersPage().then((module) => ({ default: module.UsersPage }))
);
const LogsPage = lazy(() =>
  loadLogsPage().then((module) => ({ default: module.LogsPage }))
);

function PageSuspense({
  children,
  fallback = null,
}: {
  children: ReactNode;
  fallback?: ReactNode;
}) {
  return <Suspense fallback={fallback}>{children}</Suspense>;
}

function VideoReturnPathRecorder() {
  const location = useLocation();

  useEffect(() => {
    rememberVideoReturnPath(routeToPath(location));
  }, [location.pathname, location.search, location.hash]);

  return null;
}

function VideoDetailRouteFallback() {
  const { isAdmin } = useAuth();
  const navigationType = useNavigationType();
  const scrollRootRef = usePageScrollRoot();

  // The detail component normally owns this scroll reset, but its module may
  // still be loading. Reset before the fallback paints so a click made far down
  // a listing cannot land below the visible skeleton.
  useLayoutEffect(() => {
    if (navigationType !== "POP") {
      scrollPageTo(scrollRootRef, { top: 0, behavior: "auto" });
    }
  }, [navigationType, scrollRootRef]);

  return <VideoDetailLoading isAdmin={isAdmin} />;
}

function VideoDetailRouteElement() {
  return (
    <RequireAuth>
      <PageSuspense fallback={<VideoDetailRouteFallback />}>
        <VideoDetailPage />
      </PageSuspense>
    </RequireAuth>
  );
}

function ListingRoutes({ location }: { location: Location }) {
  return (
    <Routes location={location}>
      <Route
        path="/"
        element={
          <RequireAuth>
            <PageSuspense>
              <HomePage />
            </PageSuspense>
          </RequireAuth>
        }
      />
      <Route
        path="/list"
        element={
          <RequireAuth>
            <PageSuspense>
              <ListingPage />
            </PageSuspense>
          </RequireAuth>
        }
      />
    </Routes>
  );
}

function OtherRoutes() {
  return (
    <Routes>
      <Route
        path="/login"
        element={
          <PageSuspense>
            <LoginPage />
          </PageSuspense>
        }
      />

      {/* 一次性分享页公开；具体视频和媒体请求由分享会话单独鉴权。 */}
      <Route
        path="/share"
        element={
          <PageSuspense>
            <SharedVideoPage />
          </PageSuspense>
        }
      />

      {/* 主站需要登录 */}
      <Route
        path="/shorts"
        element={
          <RequireAuth>
            <PageSuspense>
              <ShortsPage />
            </PageSuspense>
          </RequireAuth>
        }
      />
      <Route
        path="/upload"
        element={
          <RequireAuth>
            <RequireAdmin>
              <PageSuspense>
                <UploadPage />
              </PageSuspense>
            </RequireAdmin>
          </RequireAuth>
        }
      />
      <Route
        path="/video/:id"
        element={<VideoDetailRouteElement />}
      />

      {/* 管理后台也需要登录+管理员权限 */}
      <Route
        path="/admin"
        element={
          <RequireAuth>
            <RequireAdmin>
              <PageSuspense>
                <AdminLayout />
              </PageSuspense>
            </RequireAdmin>
          </RequireAuth>
        }
      >
        <Route index element={<Navigate to="/admin/drives" replace />} />
        <Route
          path="drives"
          element={
            <PageSuspense fallback={<DrivesPageLoading />}>
              <DrivesPage />
            </PageSuspense>
          }
        />
        <Route
          path="crawlers"
          element={
            <PageSuspense fallback={<CrawlersPageLoading />}>
              <CrawlersPage />
            </PageSuspense>
          }
        />
        <Route
          path="videos"
          element={
            <PageSuspense>
              <VideosPage />
            </PageSuspense>
          }
        />
        <Route
          path="tags"
          element={
            <PageSuspense>
              <TagsPage />
            </PageSuspense>
          }
        />
        <Route path="telegram" element={<PageSuspense><TelegramPage /></PageSuspense>} />
        <Route
          path="settings"
          element={
            <PageSuspense>
              <SettingsPage />
            </PageSuspense>
          }
        />
        <Route path="theme" element={<Navigate to="/admin/drives" replace />} />
        <Route
          path="backup"
          element={
            <PageSuspense>
              <BackupPage />
            </PageSuspense>
          }
        />
        <Route
          path="users"
          element={
            <PageSuspense>
              <UsersPage />
            </PageSuspense>
          }
        />
        <Route
          path="logs"
          element={
            <PageSuspense>
              <LogsPage />
            </PageSuspense>
          }
        />
      </Route>

      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

function RetainedListingSurface({
  location,
  active,
}: {
  location: Location;
  active: boolean;
}) {
  const rootRef = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    if (rootRef.current) rootRef.current.inert = !active;
  }, [active]);

  return (
    <div
      ref={rootRef}
      className="app-primary-route"
      aria-hidden={!active ? true : undefined}
    >
      <RouteActivityProvider active={active}>
        {/*
         * Always pass an explicit location so React Router's provider shape is
         * stable when the detail surface opens. The listing route therefore
         * keeps the same React instance without relying on router internals.
         */}
        <ListingRoutes location={location} />
      </RouteActivityProvider>
    </div>
  );
}

function VideoDetailForeground() {
  const scrollRootRef = useRef<HTMLDivElement>(null);
  const returnTitleRef = useRef(document.title);

  // The listing remains the document underneath this fixed foreground. Freeze
  // its exact scroll position until the detail history layer is removed.
  useDocumentScrollLock(true);

  useEffect(() => {
    return () => {
      // The retained listing does not remount, so its title effect will not
      // rerun when the foreground history layer disappears.
      document.title = returnTitleRef.current;
    };
  }, []);

  return (
    <div
      ref={scrollRootRef}
      className="video-detail-foreground"
      data-video-detail-foreground
    >
      <SkyStarfield />
      <PageScrollRootProvider scrollRootRef={scrollRootRef}>
        <Routes>
          <Route
            path="/video/:id"
            element={<VideoDetailRouteElement />}
          />
        </Routes>
      </PageScrollRootProvider>
    </div>
  );
}

export default function App() {
  const location = useLocation();
  const { status } = useAuth();
  const videoDetailMatch = useMatch("/video/:id");
  useNativeBackNavigation();
  const shouldSyncPreviews = status === "authed" &&
    (isVideoListingPath(location.pathname) || videoDetailMatch !== null);
  useEffect(() => {
    if (shouldSyncPreviews) return watchPreviewSettings();
  }, [shouldSyncPreviews]);

  const listingBackground = location.pathname.startsWith("/video/")
    ? readVideoListingBackground(location.state)
    : null;
  const activeListingLocation = isVideoListingPath(location.pathname)
    ? location
    : null;
  const listingLocation = listingBackground ?? activeListingLocation;

  return (
    <>
      {/* 星空蓝主题的固定位置星星层，仅在 data-theme="sky" 下可见 */}
      <SkyStarfield />
      <VideoReturnPathRecorder />
      {listingLocation ? (
        <RetainedListingSurface
          location={listingLocation}
          active={listingBackground === null}
        />
      ) : (
        <OtherRoutes />
      )}
      {listingBackground && <VideoDetailForeground />}
    </>
  );
}
