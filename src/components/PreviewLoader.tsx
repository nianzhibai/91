type Props = {
  onFinish: () => void;
};

export function PreviewLoader({ onFinish }: Props) {
  return (
    <span
      className="preview-loader"
      aria-hidden="true"
      onAnimationEnd={(event) => {
        if (event.animationName === "preview-startup-fade") onFinish();
      }}
    />
  );
}
