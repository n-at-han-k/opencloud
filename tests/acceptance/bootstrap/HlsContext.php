<?php declare(strict_types=1);
/**
 * This code is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License,
 * as published by the Free Software Foundation;
 * either version 3 of the License, or any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program. If not, see <http://www.gnu.org/licenses/>
 *
 */

use Behat\Behat\Context\Context;
use Behat\Behat\Hook\Scope\BeforeScenarioScope;
use Behat\Step\Given;
use Behat\Step\Then;
use Behat\Step\When;
use GuzzleHttp\Exception\GuzzleException;
use PHPUnit\Framework\Assert;
use Psr\Http\Message\ResponseInterface;
use TestHelpers\BehatHelper;
use TestHelpers\HttpRequestHelper;
use TestHelpers\WebDavHelper;

require_once 'bootstrap.php';

/**
 * Context for HLS video streaming steps.
 *
 * The interesting steps here are the ones that look inside the bytes. A
 * playlist that parses and segments that return 200 prove very little: the
 * failure modes this pipeline actually has are an ffmpeg run that writes an
 * empty fragment, and segments that are each individually fine but all claim to
 * start at t=0, so a player stacks them instead of playing them in sequence.
 * Both answer 200 with a plausible-looking body. So these steps read the fMP4
 * boxes and check that every segment says where it sits, and that where it says
 * it sits is where the playlist promised it would be.
 */
class HlsContext implements Context {
	private FeatureContext $featureContext;

	/**
	 * Media segments smaller than this are empty fragments. A real segment of
	 * even the tiniest test video is several kilobytes; an ffmpeg run that
	 * produced nothing writes a couple of dozen bytes of box headers.
	 */
	private const MIN_SEGMENT_BYTES = 500;

	/**
	 * Segment start times are compared against the playlist's own EXTINF sums.
	 * The tolerance covers a frame or two of boundary rounding, not a segment
	 * landing in the wrong place.
	 */
	private const START_TOLERANCE_SECONDS = 0.25;

	/**
	 * @BeforeScenario
	 *
	 * @param BeforeScenarioScope $scope
	 *
	 * @return void
	 */
	public function before(BeforeScenarioScope $scope): void {
		$environment = $scope->getEnvironment();
		$this->featureContext = BehatHelper::getContext($scope, $environment, 'FeatureContext');
	}

	/**
	 * @param string $user
	 * @param string $path
	 * @param string $artifact
	 *
	 * @return ResponseInterface
	 * @throws GuzzleException
	 */
	public function downloadHlsArtifact(string $user, string $path, string $artifact): ResponseInterface {
		$user = $this->featureContext->getActualUsername($user);

		return $this->featureContext->makeDavRequest(
			$user,
			"GET",
			$path,
			[],
			null,
			null,
			"files",
			null,
			false,
			$this->featureContext->getPasswordForUser($user),
			['hls' => $artifact]
		);
	}

	/**
	 * @param string $user
	 * @param string $artifact
	 * @param string $path
	 *
	 * @return void
	 * @throws GuzzleException
	 */
	#[When('user :user downloads the HLS artifact :artifact of :path using the WebDAV API')]
	#[When('user :user tries to download the HLS artifact :artifact of :path using the WebDAV API')]
	public function userDownloadsHlsArtifact(string $user, string $artifact, string $path): void {
		$this->featureContext->setResponse($this->downloadHlsArtifact($user, $path, $artifact));
		$this->featureContext->pushToLastStatusCodesArrays();
	}

	/**
	 * @param string $user
	 * @param string $artifact
	 * @param string $path
	 *
	 * @return void
	 * @throws GuzzleException
	 */
	#[Given('user :user has downloaded the HLS artifact :artifact of :path')]
	public function userHasDownloadedHlsArtifact(string $user, string $artifact, string $path): void {
		$response = $this->downloadHlsArtifact($user, $path, $artifact);
		$this->featureContext->theHTTPStatusCodeShouldBe(200, "", $response);
		$this->featureContext->setResponse($response);
	}

	/**
	 * @param string $artifact
	 * @param string $password
	 *
	 * @return ResponseInterface
	 * @throws GuzzleException
	 */
	public function downloadHlsArtifactFromPublicShare(string $artifact, string $password = ""): ResponseInterface {
		$token = ($this->featureContext->isUsingSharingNG())
		? $this->featureContext->shareNgGetLastCreatedLinkShareToken()
		: $this->featureContext->getLastCreatedPublicShareToken();
		$davPath = WebDavHelper::getDavPath(WebDavHelper::DAV_VERSION_NEW, $token, "public-files");
		$fullUrl = $this->featureContext->getBaseUrl() . "/$davPath/?" . \http_build_query(['hls' => $artifact]);
		$password = $this->featureContext->getActualPassword($password);

		return HttpRequestHelper::get(
			$fullUrl,
			$this->featureContext->getStepLineRef(),
			$password === "" ? "" : "public",
			$password,
			['X-Requested-With' => 'XMLHttpRequest']
		);
	}

	/**
	 * @param string $artifact
	 *
	 * @return void
	 * @throws GuzzleException
	 */
	#[When('the public downloads the HLS artifact :artifact from the last created public link using the WebDAV API')]
	public function publicDownloadsHlsArtifact(string $artifact): void {
		$this->featureContext->setResponse($this->downloadHlsArtifactFromPublicShare($artifact));
		$this->featureContext->pushToLastStatusCodesArrays();
	}

	/**
	 * @return void
	 */
	#[Then('the response should be an HLS multivariant playlist')]
	public function theResponseShouldBeAnHlsMultivariantPlaylist(): void {
		$body = $this->responseBody();

		Assert::assertSame(
			"application/vnd.apple.mpegurl",
			$this->responseContentType(),
			"a playlist served under the wrong media type is refused by Safari"
		);
		Assert::assertStringStartsWith("#EXTM3U", $body, "response is not an m3u8 playlist");
		Assert::assertStringContainsString(
			"#EXT-X-VERSION:7",
			$body,
			"fMP4 segments need protocol version 7 or later"
		);
		Assert::assertStringContainsString("#EXT-X-STREAM-INF:", $body, "playlist offers no rendition");
		Assert::assertStringNotContainsString(
			"#EXTINF:",
			$body,
			"a multivariant playlist must not list segments itself"
		);
	}

	/**
	 * @param string $codecs
	 *
	 * @return void
	 */
	#[Then('the HLS multivariant playlist should offer codecs :codecs')]
	public function theHlsMultivariantPlaylistShouldOfferCodecs(string $codecs): void {
		Assert::assertStringContainsString(
			'CODECS="' . $codecs . '"',
			$this->responseBody(),
			"the playlist does not advertise the expected codecs"
		);
	}

	/**
	 * @param int $count
	 *
	 * @return void
	 */
	#[Then('the response should be an HLS media playlist with :count segments')]
	public function theResponseShouldBeAnHlsMediaPlaylistWithSegments(int $count): void {
		$playlist = $this->parseMediaPlaylist($this->responseBody());

		Assert::assertCount(
			$count,
			$playlist['segments'],
			"unexpected number of segments in the media playlist"
		);
	}

	/**
	 * @return void
	 */
	#[Then('the response should be a valid HLS initialization segment')]
	public function theResponseShouldBeAValidHlsInitializationSegment(): void {
		$body = $this->responseBody();
		$types = $this->topLevelBoxTypes($body);

		Assert::assertContains("ftyp", $types, "initialization segment has no ftyp box: " . \implode(",", $types));
		Assert::assertContains("moov", $types, "initialization segment has no moov box: " . \implode(",", $types));
		Assert::assertNotContains(
			"moof",
			$types,
			"the initialization segment carries media data; it should describe the tracks only"
		);
		Assert::assertNotEmpty(
			$this->trackTimescales($body),
			"no track timescales could be read from the initialization segment"
		);
	}

	/**
	 * @return void
	 */
	#[Then('the response should be a valid HLS media segment')]
	public function theResponseShouldBeAValidHlsMediaSegment(): void {
		$body = $this->responseBody();
		$types = $this->topLevelBoxTypes($body);

		Assert::assertGreaterThan(
			self::MIN_SEGMENT_BYTES,
			\strlen($body),
			"the segment is too small to hold any media; ffmpeg wrote an empty fragment"
		);
		Assert::assertContains("moof", $types, "media segment has no moof box: " . \implode(",", $types));
		Assert::assertContains("mdat", $types, "media segment has no mdat box: " . \implode(",", $types));
		Assert::assertNotContains(
			"moov",
			$types,
			"the media segment repeats the track description; that belongs in the initialization segment"
		);
	}

	/**
	 * Walk a whole stream the way a player does and check it is coherent.
	 *
	 * This is the step that earns the suite its keep. It fetches the
	 * multivariant playlist, the media playlist it points at, the
	 * initialization segment named by EXT-X-MAP and every media segment, and
	 * then checks the thing that matters: that each segment's own track fragment
	 * decode time places it exactly where the playlist's EXTINF values said it
	 * would be. A pipeline that returns 200 for everything but writes the same
	 * decode time into every segment passes every other check here and plays
	 * four seconds of video on a loop.
	 *
	 * @param string $path
	 * @param string $user
	 * @param string $duration
	 *
	 * @return void
	 * @throws GuzzleException
	 */
	#[Then('the HLS stream of :path for user :user should be one continuous video of :duration seconds')]
	public function theHlsStreamShouldBeOneContinuousVideo(string $path, string $user, string $duration): void {
		$expectedDuration = (float) $duration;

		$master = $this->fetchArtifactBody($user, $path, "master.m3u8");
		$variant = $this->parseVariantArtifact($master);

		$playlist = $this->parseMediaPlaylist($this->fetchArtifactBody($user, $path, $variant));
		Assert::assertNotEmpty($playlist['segments'], "the media playlist lists no segments");
		Assert::assertNotEmpty($playlist['map'], "the media playlist has no EXT-X-MAP");

		$init = $this->fetchArtifactBody($user, $path, $playlist['map']);
		$timescales = $this->trackTimescales($init);
		Assert::assertNotEmpty($timescales, "no track timescales in the initialization segment");

		$expectedStart = 0.0;
		$previousStart = -1.0;

		foreach ($playlist['segments'] as $index => $segment) {
			$body = $this->fetchArtifactBody($user, $path, $segment['uri']);

			Assert::assertGreaterThan(
				self::MIN_SEGMENT_BYTES,
				\strlen($body),
				"segment $index ({$segment['uri']}) is " . \strlen($body) .
				" bytes, which means ffmpeg produced an empty fragment"
			);

			$decodeTimes = $this->decodeTimes($body, $timescales);
			Assert::assertNotEmpty(
				$decodeTimes,
				"segment $index ({$segment['uri']}) carries no track fragment decode time, " .
				"so a player cannot place it on the timeline"
			);

			foreach ($decodeTimes as $trackId => $start) {
				Assert::assertEqualsWithDelta(
					$expectedStart,
					$start,
					self::START_TOLERANCE_SECONDS,
					"segment $index, track $trackId reports that it starts at {$start}s, but the " .
					"playlist places it at {$expectedStart}s. Segments that disagree with the " .
					"playlist do not play in sequence."
				);
			}

			$videoStart = \reset($decodeTimes);
			Assert::assertGreaterThan(
				$previousStart,
				$videoStart,
				"segment $index starts at or before segment " . ($index - 1) .
				"; the segments are not advancing along the timeline"
			);
			$previousStart = $videoStart;

			$expectedStart += $segment['duration'];
		}

		Assert::assertEqualsWithDelta(
			$expectedDuration,
			$expectedStart,
			0.5,
			"the playlist covers {$expectedStart}s of video, but the source is {$expectedDuration}s long"
		);

		Assert::assertGreaterThanOrEqual(
			(int) \ceil(\max(\array_column($playlist['segments'], 'duration'))),
			$playlist['targetDuration'],
			"EXT-X-TARGETDURATION is smaller than the longest segment, which RFC 8216 forbids"
		);
	}

	/**
	 * @param string $user
	 * @param string $path
	 * @param string $artifact
	 *
	 * @return string
	 * @throws GuzzleException
	 */
	private function fetchArtifactBody(string $user, string $path, string $artifact): string {
		$response = $this->downloadHlsArtifact($user, $path, $artifact);

		Assert::assertSame(
			200,
			$response->getStatusCode(),
			"requesting HLS artifact '$artifact' of '$path' answered " . $response->getStatusCode()
		);

		return (string) $response->getBody();
	}

	/**
	 * @return string
	 */
	private function responseBody(): string {
		return (string) $this->featureContext->getResponse()->getBody();
	}

	/**
	 * @return string
	 */
	private function responseContentType(): string {
		$header = $this->featureContext->getResponse()->getHeader("Content-Type");

		return \explode(";", $header[0] ?? "")[0];
	}

	/**
	 * Read the rendition's media playlist artifact out of a multivariant
	 * playlist. Playlist URIs are query-only references ("?hls=v0/index.m3u8"),
	 * because the artifacts all live on the source file's own DAV URL.
	 *
	 * @param string $master
	 *
	 * @return string
	 */
	private function parseVariantArtifact(string $master): string {
		$lines = \preg_split('/\R/', $master);
		foreach ($lines as $index => $line) {
			if (\str_starts_with($line, "#EXT-X-STREAM-INF:")) {
				$uri = \trim($lines[$index + 1] ?? "");
				Assert::assertNotSame("", $uri, "EXT-X-STREAM-INF is not followed by a URI");

				return $this->artifactFromUri($uri);
			}
		}

		Assert::fail("no EXT-X-STREAM-INF in the multivariant playlist:\n$master");
	}

	/**
	 * @param string $uri
	 *
	 * @return string
	 */
	private function artifactFromUri(string $uri): string {
		Assert::assertStringStartsWith(
			"?hls=",
			$uri,
			"playlist URI '$uri' is not a query-only reference. A plain relative path would " .
			"resolve against the file's DAV path and drop the filename."
		);

		\parse_str(\substr($uri, 1), $query);
		Assert::assertArrayHasKey("hls", $query, "playlist URI '$uri' carries no hls parameter");

		return (string) $query["hls"];
	}

	/**
	 * @param string $body
	 *
	 * @return array{segments: list<array{uri: string, duration: float}>, map: string, targetDuration: int}
	 */
	private function parseMediaPlaylist(string $body): array {
		Assert::assertStringStartsWith("#EXTM3U", $body, "response is not an m3u8 playlist");
		Assert::assertStringContainsString(
			"#EXT-X-PLAYLIST-TYPE:VOD",
			$body,
			"a playlist for a stored file should be a VOD playlist, so a player can seek freely"
		);
		Assert::assertStringContainsString(
			"#EXT-X-ENDLIST",
			$body,
			"a VOD playlist without EXT-X-ENDLIST leaves the player waiting for more segments"
		);

		$segments = [];
		$map = "";
		$targetDuration = 0;
		$pendingDuration = null;

		foreach (\preg_split('/\R/', $body) as $line) {
			$line = \trim($line);
			if ($line === "") {
				continue;
			}
			if (\str_starts_with($line, "#EXT-X-MAP:")) {
				\preg_match('/URI="([^"]+)"/', $line, $matches);
				Assert::assertNotEmpty($matches, "EXT-X-MAP has no URI attribute: $line");
				$map = $this->artifactFromUri($matches[1]);
				continue;
			}
			if (\str_starts_with($line, "#EXT-X-TARGETDURATION:")) {
				$targetDuration = (int) \substr($line, \strlen("#EXT-X-TARGETDURATION:"));
				continue;
			}
			if (\str_starts_with($line, "#EXTINF:")) {
				$pendingDuration = (float) \rtrim(\substr($line, \strlen("#EXTINF:")), ",");
				continue;
			}
			if (\str_starts_with($line, "#")) {
				continue;
			}
			Assert::assertNotNull($pendingDuration, "segment URI '$line' has no EXTINF before it");
			$segments[] = ['uri' => $this->artifactFromUri($line), 'duration' => $pendingDuration];
			$pendingDuration = null;
		}

		Assert::assertGreaterThan(0, $targetDuration, "the media playlist has no EXT-X-TARGETDURATION");

		return ['segments' => $segments, 'map' => $map, 'targetDuration' => $targetDuration];
	}

	/**
	 * Iterate the ISO base media file format boxes in a byte range.
	 *
	 * @param string $data
	 * @param int $start
	 * @param int $end
	 *
	 * @return list<array{type: string, bodyStart: int, boxEnd: int}>
	 */
	private function boxes(string $data, int $start = 0, ?int $end = null): array {
		$end = $end ?? \strlen($data);
		$found = [];
		$offset = $start;

		while ($offset + 8 <= $end) {
			$size = \unpack("N", \substr($data, $offset, 4))[1];
			$type = \substr($data, $offset + 4, 4);
			$header = 8;

			if ($size === 1) {
				if ($offset + 16 > $end) {
					break;
				}
				$size = \unpack("J", \substr($data, $offset + 8, 8))[1];
				$header = 16;
			} elseif ($size === 0) {
				$size = $end - $offset;
			}

			if ($size < $header || $offset + $size > $end) {
				break;
			}

			$found[] = ['type' => $type, 'bodyStart' => $offset + $header, 'boxEnd' => $offset + $size];
			$offset += $size;
		}

		return $found;
	}

	/**
	 * @param string $data
	 * @param int $start
	 * @param int $end
	 * @param string $type
	 *
	 * @return list<array{type: string, bodyStart: int, boxEnd: int}>
	 */
	private function boxesOfType(string $data, int $start, int $end, string $type): array {
		return \array_values(
			\array_filter(
				$this->boxes($data, $start, $end),
				static fn (array $box): bool => $box['type'] === $type
			)
		);
	}

	/**
	 * @param string $data
	 *
	 * @return list<string>
	 */
	private function topLevelBoxTypes(string $data): array {
		return \array_column($this->boxes($data), 'type');
	}

	/**
	 * Read each track's id and timescale out of an initialization segment. A
	 * fragment's decode time is expressed in its own track's timescale, so the
	 * two have to be read together to mean anything in seconds.
	 *
	 * @param string $init
	 *
	 * @return array<int, int> track id => timescale
	 */
	private function trackTimescales(string $init): array {
		$scales = [];

		foreach ($this->boxesOfType($init, 0, \strlen($init), "moov") as $moov) {
			foreach ($this->boxesOfType($init, $moov['bodyStart'], $moov['boxEnd'], "trak") as $trak) {
				$trackId = null;
				$timescale = null;

				foreach ($this->boxesOfType($init, $trak['bodyStart'], $trak['boxEnd'], "tkhd") as $tkhd) {
					// version(1) flags(3), creation and modification times, then
					// the track id. The times are 8 bytes each in version 1.
					$offset = $tkhd['bodyStart'] + 4 + (\ord($init[$tkhd['bodyStart']]) === 1 ? 16 : 8);
					$trackId = \unpack("N", \substr($init, $offset, 4))[1];
				}

				foreach ($this->boxesOfType($init, $trak['bodyStart'], $trak['boxEnd'], "mdia") as $mdia) {
					foreach ($this->boxesOfType($init, $mdia['bodyStart'], $mdia['boxEnd'], "mdhd") as $mdhd) {
						$offset = $mdhd['bodyStart'] + 4 + (\ord($init[$mdhd['bodyStart']]) === 1 ? 16 : 8);
						$timescale = \unpack("N", \substr($init, $offset, 4))[1];
					}
				}

				if ($trackId !== null && $timescale) {
					$scales[$trackId] = $timescale;
				}
			}
		}

		return $scales;
	}

	/**
	 * Read each track's decode time out of a media segment, in seconds. This is
	 * the number that says where the segment belongs on the timeline.
	 *
	 * @param string $segment
	 * @param array<int, int> $timescales
	 *
	 * @return array<int, float> track id => start time in seconds
	 */
	private function decodeTimes(string $segment, array $timescales): array {
		$starts = [];

		foreach ($this->boxesOfType($segment, 0, \strlen($segment), "moof") as $moof) {
			foreach ($this->boxesOfType($segment, $moof['bodyStart'], $moof['boxEnd'], "traf") as $traf) {
				$trackId = null;

				foreach ($this->boxesOfType($segment, $traf['bodyStart'], $traf['boxEnd'], "tfhd") as $tfhd) {
					$trackId = \unpack("N", \substr($segment, $tfhd['bodyStart'] + 4, 4))[1];
				}
				if ($trackId === null || !isset($timescales[$trackId]) || isset($starts[$trackId])) {
					// Only the first fragment of each track says where the
					// segment starts; later ones continue from it.
					continue;
				}

				foreach ($this->boxesOfType($segment, $traf['bodyStart'], $traf['boxEnd'], "tfdt") as $tfdt) {
					$version = \ord($segment[$tfdt['bodyStart']]);
					$base = $version === 1
					? \unpack("J", \substr($segment, $tfdt['bodyStart'] + 4, 8))[1]
					: \unpack("N", \substr($segment, $tfdt['bodyStart'] + 4, 4))[1];
					$starts[$trackId] = $base / $timescales[$trackId];
				}
			}
		}

		return $starts;
	}
}
